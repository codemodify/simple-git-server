package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

type gitService string

const (
	serviceReceivePack gitService = "receive-pack"
	serviceUploadPack  gitService = "upload-pack"
)

type parsedGitCommand struct {
	service gitService
	repo    string // raw repo argument from the client (quotes stripped)
}

func handleSession(channel ssh.Channel, requests <-chan *ssh.Request, cfg *sshServerConfig, remote string) {
	defer channel.Close()

	for req := range requests {
		switch req.Type {
		case "env":
			// ignore client env; reply success so git clients continue
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "exec":
			cmdLine, ok := parseExecPayload(req.Payload)
			if !ok {
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
				writeExitStatus(channel, 128)
				return
			}
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
			code := runGitCommand(channel, cfg, cmdLine, remote)
			writeExitStatus(channel, code)
			return
		case "shell":
			// no interactive shell
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-push: shell disabled; use git receive-pack / upload-pack only\n")
			writeExitStatus(channel, 128)
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func parseExecPayload(payload []byte) (string, bool) {
	// SSH exec payload: uint32 length + command string
	if len(payload) < 4 {
		return "", false
	}
	n := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if n < 0 || 4+n > len(payload) {
		return "", false
	}
	return string(payload[4 : 4+n]), true
}

func writeExitStatus(channel ssh.Channel, code int) {
	// exit-status request: uint32 status
	var status [4]byte
	status[0] = byte(uint32(code) >> 24)
	status[1] = byte(uint32(code) >> 16)
	status[2] = byte(uint32(code) >> 8)
	status[3] = byte(uint32(code))
	_, _ = channel.SendRequest("exit-status", false, status[:])
}

func runGitCommand(channel ssh.Channel, cfg *sshServerConfig, cmdLine, remote string) int {
	parsed, err := parseGitSSHCommand(cmdLine, cfg.enableSSHWrite)
	if err != nil {
		logInfo("git    | %s reject command %q: %v", remote, cmdLine, err)
		_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-push: "+err.Error()+"\n")
		return 128
	}

	repoPath, err := resolveRepoPath(cfg.gitReposFolder, parsed.repo)
	if err != nil {
		logInfo("git    | %s reject path %q: %v", remote, parsed.repo, err)
		_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-push: "+err.Error()+"\n")
		return 128
	}

	gitArg := "receive-pack"
	if parsed.service == serviceUploadPack {
		gitArg = "upload-pack"
	}

	logInfo("git    | %s %s %s", remote, gitArg, repoPath)

	cmd := exec.Command("git", gitArg, repoPath)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_DIR=" + repoPath,
	}
	cmd.Stdin = channel
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		logInfo("git    | %s exec error: %v", remote, err)
		return 128
	}
	return 0
}

// parseGitSSHCommand accepts only:
//
//	git-receive-pack 'repo.git'  /  git receive-pack 'repo.git'
//	git-upload-pack 'repo.git'   /  git upload-pack 'repo.git'  (if enableSSHWrite)
func parseGitSSHCommand(cmdLine string, enableSSHWrite bool) (*parsedGitCommand, error) {
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" {
		return nil, fmt.Errorf("empty command")
	}

	parts := splitGitCommand(cmdLine)
	if len(parts) < 2 {
		return nil, fmt.Errorf("unsupported command")
	}

	var service gitService
	var repoArg string

	switch {
	case parts[0] == "git-receive-pack" && len(parts) == 2:
		service = serviceReceivePack
		repoArg = parts[1]
	case parts[0] == "git-upload-pack" && len(parts) == 2:
		service = serviceUploadPack
		repoArg = parts[1]
	case parts[0] == "git" && len(parts) == 3 && parts[1] == "receive-pack":
		service = serviceReceivePack
		repoArg = parts[2]
	case parts[0] == "git" && len(parts) == 3 && parts[1] == "upload-pack":
		service = serviceUploadPack
		repoArg = parts[2]
	default:
		return nil, fmt.Errorf("unsupported command")
	}

	if service == serviceUploadPack && !enableSSHWrite {
		return nil, fmt.Errorf("upload-pack disabled")
	}

	repoArg = stripQuotes(repoArg)
	if repoArg == "" {
		return nil, fmt.Errorf("empty repository path")
	}

	return &parsedGitCommand{service: service, repo: repoArg}, nil
}

func stripQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// splitGitCommand splits on whitespace but keeps single/double-quoted segments intact.
func splitGitCommand(s string) []string {
	var parts []string
	var cur strings.Builder
	var inQuote rune // 0, '\'', or '"'

	flush := func() {
		if cur.Len() > 0 {
			parts = append(parts, cur.String())
			cur.Reset()
		}
	}

	for _, r := range s {
		switch {
		case inQuote != 0:
			if r == inQuote {
				inQuote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			inQuote = r
			cur.WriteRune(r)
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return parts
}

// resolveRepoPath joins repo under root with path clean; rejects ".."; ensures .git suffix.
// When checkExists is false (unit tests), Stat is skipped so path logic can be tested without a real repo.
func resolveRepoPath(root, repo string) (string, error) {
	return resolveRepoPathOpts(root, repo, true)
}

func resolveRepoPathOpts(root, repo string, checkExists bool) (string, error) {
	repo = strings.TrimSpace(repo)
	repo = strings.TrimPrefix(repo, "/")
	repo = strings.TrimPrefix(repo, "./")

	if repo == "" {
		return "", fmt.Errorf("empty repository path")
	}
	if strings.Contains(repo, "..") {
		return "", fmt.Errorf("invalid repository path")
	}

	cleaned := filepath.Clean(repo)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid repository path")
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute repository path not allowed")
	}
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("invalid repository path")
	}

	if !strings.HasSuffix(cleaned, ".git") {
		cleaned = cleaned + ".git"
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	full := filepath.Join(absRoot, cleaned)
	full = filepath.Clean(full)

	// Ensure full stays under absRoot
	rel, err := filepath.Rel(absRoot, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("repository path escapes gitReposFolder")
	}

	if checkExists {
		info, err := os.Stat(full)
		if err != nil {
			return "", fmt.Errorf("repository not found")
		}
		if !info.IsDir() {
			return "", fmt.Errorf("repository not a directory")
		}
	}

	return full, nil
}
