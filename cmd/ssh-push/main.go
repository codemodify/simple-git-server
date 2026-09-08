package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

var version = "dev"

const (
	banner       = "codemodify/simple-git-server-ssh-push"
	defaultRepos = "."
	defaultSSHOn = "127.0.0.1:64222"
)

func init() {
	log.SetFlags(0)
}

func main() {
	showVersion := flag.Bool("version", false, "print version")

	gitReposFolder := flag.String("git-repos-folder", defaultRepos, "root folder for git repos")
	sshOn := flag.String("ssh-on", defaultSSHOn, "SSH bind address")
	sshHostKey := flag.String("ssh-host-key", "", "path to PEM/OpenSSH private host key (required)")
	sshAuthorizedKeys := flag.String("ssh-authorized-keys", "", "path to authorized_keys file (required)")
	enableSSHWrite := flag.Bool("enable-ssh-write", true, "allow upload-pack (fetch/clone) over SSH")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if *sshHostKey == "" {
		logError("'--ssh-host-key' is required")
	}
	if *sshAuthorizedKeys == "" {
		logError("'--ssh-authorized-keys' is required")
	}

	if _, err := exec.LookPath("git"); err != nil {
		logError("git binary not found on PATH (required for receive-pack/upload-pack): %v", err)
	}

	hostKeySigner, err := loadHostKey(*sshHostKey)
	if err != nil {
		logError("load host key: %v", err)
	}

	authorized, err := loadAuthorizedKeys(*sshAuthorizedKeys)
	if err != nil {
		logError("load authorized_keys: %v", err)
	}
	if len(authorized) == 0 {
		logError("no usable public keys in %s", *sshAuthorizedKeys)
	}

	cfg := &sshServerConfig{
		gitReposFolder:   *gitReposFolder,
		enableSSHWrite: *enableSSHWrite,
	}

	sshConfig := &ssh.ServerConfig{
		PublicKeyCallback: publicKeyCallback(authorized),
		ServerVersion:     "SSH-2.0-simple-git-server-ssh-push",
	}
	sshConfig.AddHostKey(hostKeySigner)

	listener, err := net.Listen("tcp", *sshOn)
	if err != nil {
		logError("listen on %s: %v", *sshOn, err)
	}

	logInfo("listen | ssh://%s", *sshOn)
	logInfo("gitReposFolder | %s", cfg.gitReposFolder)
	logInfo("auth   | authorized_keys=%s (%d key(s))", *sshAuthorizedKeys, len(authorized))
	logInfo("git    | receive-pack always; upload-pack=%v", cfg.enableSSHWrite)

	errCh := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				errCh <- fmt.Errorf("accept: %w", err)
				return
			}
			go handleConn(conn, sshConfig, cfg)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		logInfo("err    | %v", err)
		_ = listener.Close()
		os.Exit(1)
	case sig := <-sigCh:
		logInfo("signal | %s", sig)
		_ = listener.Close()
	}
}

type sshServerConfig struct {
	gitReposFolder   string
	enableSSHWrite bool
}

func loadHostKey(path string) (ssh.Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, err
	}
	return signer, nil
}

func handleConn(nConn net.Conn, sshConfig *ssh.ServerConfig, cfg *sshServerConfig) {
	defer nConn.Close()

	remote := nConn.RemoteAddr().String()
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, sshConfig)
	if err != nil {
		logInfo("ssh    | %s handshake failed: %v", remote, err)
		return
	}
	defer sConn.Close()

	logInfo("ssh    | %s connected user=%s", remote, sConn.User())
	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			logInfo("ssh    | %s accept channel: %v", remote, err)
			continue
		}
		go handleSession(channel, requests, cfg, remote)
	}
	logInfo("ssh    | %s disconnected", remote)
}

func logInfo(format string, v ...interface{}) {
	log.Default().Printf("I | %s | %s | %s | %s \n", banner, version, timeNowAsString(), fmt.Sprintf(format, v...))
}

func logError(format string, v ...interface{}) {
	log.Default().Fatalf("E | %s | %s | %s | %s \n", banner, version, timeNowAsString(), fmt.Sprintf(format, v...))
}

func timeNowAsString() string {
	return time.Now().UTC().Format(time.RFC3339)
}
