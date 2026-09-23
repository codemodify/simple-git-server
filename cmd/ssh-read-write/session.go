package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

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

func handleSession(channel ssh.Channel, requests <-chan *ssh.Request, cfg *sshServerConfig, remote string, act *connActivity) {
	defer channel.Close()

	// A channel that is accepted and then never carries an exec or shell request
	// would block this goroutine forever. That holds a session slot and, because
	// the connection watchdog counts any live session as activity, keeps the whole
	// connection pinned so it is never reaped either.
	guard := time.AfterFunc(cfg.sessionRequestDeadline(), func() {
		logInfo("ssh    | %s session idle %s with no exec, closing channel", remote, cfg.sessionRequestDeadline())
		_ = channel.Close()
	})
	defer guard.Stop()

	for req := range requests {
		switch req.Type {
		case "env":
			// ignore client env; reply success so git clients continue
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		case "exec":
			guard.Stop()
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
			code := runGitCommand(channel, cfg, cmdLine, remote, act)
			writeExitStatus(channel, code)
			return
		case "shell":
			guard.Stop()
			// no interactive shell
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-read-write: shell disabled; use git receive-pack / upload-pack only\n")
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

func runGitCommand(channel ssh.Channel, cfg *sshServerConfig, cmdLine, remote string, act *connActivity) int {
	parsed, err := parseGitSSHCommand(cmdLine, cfg.enableSSHRead, cfg.enableSSHWrite)
	if err != nil {
		logInfo("git    | %s reject command %q: %v", remote, cmdLine, err)
		_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-read-write: "+err.Error()+"\n")
		return 128
	}

	repoPath, err := resolveRepoPath(cfg.gitReposFolder, parsed.repo)
	if err != nil {
		logInfo("git    | %s reject path %q: %v", remote, parsed.repo, err)
		_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-read-write: "+err.Error()+"\n")
		return 128
	}

	gitArg := "receive-pack"
	if parsed.service == serviceUploadPack {
		gitArg = "upload-pack"
	}

	// Bound git processes globally: the per-connection session limit alone still
	// permits maxConns*maxSessionsPerConn children.
	if cfg.gitSem != nil {
		select {
		case cfg.gitSem <- struct{}{}:
			defer func() { <-cfg.gitSem }()
		default:
			logInfo("git    | %s rejected: concurrent git process limit reached", remote)
			_, _ = io.WriteString(channel.Stderr(), "simple-git-server-ssh-read-write: server busy, too many concurrent git processes\n")
			return 128
		}
	}

	// Only count real work as connection activity, and only from here: command
	// parsing and its error writes happen above, and marking those active would
	// let a blocked error write exempt the connection from the idle watchdog.
	act.begin()
	defer act.end()
	cfg.gitWG.Add(1)
	defer cfg.gitWG.Done()

	logInfo("git    | %s %s %s", remote, gitArg, repoPath)

	// A session that starts git and then stops moving bytes would otherwise hold
	// its slot until the client disconnects; one such session is enough to deny
	// service at a low --git-max-concurrent. Cancel on lack of progress, not on
	// total wall time, so long transfers are unaffected.
	ctx, cancel := context.WithCancel(cfg.gitBase())
	defer cancel()
	prog := &progress{}
	prog.mark()
	stop := make(chan struct{})
	defer close(stop)
	go watchGitStall(prog, cfg.gitIdleTimeout, channel, cancel, stop, remote)

	cmd := exec.CommandContext(ctx, "git", gitArg, repoPath)
	// git upload-pack forks git pack-objects; cancellation must reach the whole
	// tree or the grandchild keeps writing to the channel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = gitWaitDelay
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_DIR=" + repoPath,
	}
	cmd.Stdin = &progressReader{r: channel, p: prog}
	cmd.Stdout = &progressWriter{w: channel, p: prog}
	// stderr counts as progress: a server-side hook that only writes progress to
	// stderr must not look like a stalled session.
	cmd.Stderr = &progressWriter{w: channel.Stderr(), p: prog}

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		logInfo("git    | %s exec error: %v", remote, err)
		return 128
	}
	return 0
}

const gitWaitDelay = 5 * time.Second

// progress records when a git session last moved bytes in either direction.
type progress struct{ last atomic.Int64 }

func (p *progress) mark()                  { p.last.Store(time.Now().UnixNano()) }
func (p *progress) idleFor() time.Duration { return time.Since(time.Unix(0, p.last.Load())) }

type progressReader struct {
	r io.Reader
	p *progress
}

func (t *progressReader) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if n > 0 {
		t.p.mark()
	}
	return n, err
}

type progressWriter struct {
	w io.Writer
	p *progress
}

func (t *progressWriter) Write(b []byte) (int, error) {
	n, err := t.w.Write(b)
	if n > 0 {
		t.p.mark()
	}
	return n, err
}

// watchGitStall cancels the git command if neither direction has moved for idle.
//
// Killing the child is not sufficient on its own: cmd.Run then blocks in Wait on
// the os/exec goroutine still copying from the SSH channel, so the session would
// keep its --git-max-concurrent slot until the client disconnected. Closing the
// channel fails that pending read.
func watchGitStall(p *progress, idle time.Duration, channel ssh.Channel, cancel context.CancelFunc, stop <-chan struct{}, remote string) {
	if idle <= 0 {
		return
	}
	tick := idle / 4
	if tick < 100*time.Millisecond {
		tick = 100 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if p.idleFor() >= idle {
				logInfo("git    | %s no progress for %s, cancelling git", remote, idle)
				cancel()
				if channel != nil {
					_ = channel.Close()
				}
				return
			}
		}
	}
}

// parseGitSSHCommand accepts only:
//
//	git-upload-pack 'repo.git'   /  git upload-pack 'repo.git'   (read;  requires allowFetch)
//	git-receive-pack 'repo.git'  /  git receive-pack 'repo.git'  (write; requires allowPush)
//
// upload-pack is the READ side (fetch/clone) and receive-pack is the WRITE side
// (push). Each is gated by its own capability; neither is ever implicit.
func parseGitSSHCommand(cmdLine string, allowFetch, allowPush bool) (*parsedGitCommand, error) {
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

	if service == serviceUploadPack && !allowFetch {
		return nil, fmt.Errorf("fetch (git-upload-pack) is disabled: --enable-ssh-read is off")
	}
	if service == serviceReceivePack && !allowPush {
		return nil, fmt.Errorf("push (git-receive-pack) is disabled: --enable-ssh-write is off")
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

// withinRoot reports whether target, with symlinks resolved, is inside root.
func withinRoot(root, target string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving git repos folder: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return fmt.Errorf("repository not found")
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("repository path escapes gitReposFolder")
	}
	return nil
}

// hasDotDotSegment reports whether any segment of p is exactly "..".
// A strings.Contains(p, "..") check would also reject legitimate names such as
// "weird..name.git", so segments are compared exactly.
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
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
	if hasDotDotSegment(repo) {
		return "", fmt.Errorf("invalid repository path")
	}

	cleaned := filepath.Clean(repo)
	if cleaned == "." || hasDotDotSegment(cleaned) {
		return "", fmt.Errorf("invalid repository path")
	}
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("absolute repository path not allowed")
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
		// The checks above are lexical. A symlink planted under the root would
		// still resolve outside it, so re-check containment after resolution.
		if err := withinRoot(absRoot, full); err != nil {
			return "", err
		}
	}

	return full, nil
}
