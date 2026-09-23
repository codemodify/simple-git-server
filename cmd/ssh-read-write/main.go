package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

var version = "dev"

const (
	banner       = "codemodify/simple-git-server-ssh-read-write"
	defaultRepos = "/home/git"
	defaultSSHOn = "127.0.0.1:64222"

	// Pre-auth resource bounds. Without these an unauthenticated client can hold
	// a socket (and goroutine) open forever by never completing the handshake.
	sshHandshakeTimeout = 30 * time.Second
	maxSessionsPerConn  = 8

	defaultMaxSSHConns      = 128
	defaultGitMaxConcurrent = 32
	defaultGitIdleTimeout   = 60 * time.Second

	// gitShutdownGrace bounds how long shutdown waits for cancelled git commands
	// (and their hooks) to actually die before giving up.
	gitShutdownGrace = 3 * time.Second
)

const (
	// defaultConnIdleTimeout is the post-auth bound. The handshake deadline is
	// cleared once authenticated, so without this an authenticated client can
	// hold connection slots forever by never doing any work.
	defaultConnIdleTimeout = 2 * time.Minute

	// defaultSessionRequestTimeout bounds how long an accepted session channel
	// may sit without an exec/shell request. Real git clients send one at once.
	defaultSessionRequestTimeout = 30 * time.Second
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
	enableSSHRead := flag.Bool("enable-ssh-read", true, "allow git-upload-pack (clone/fetch) over SSH")
	enableSSHWrite := flag.Bool("enable-ssh-write", false, "allow git-receive-pack (push) over SSH")
	gitMaxConcurrent := flag.Int("git-max-concurrent", defaultGitMaxConcurrent, "max concurrent git processes across all SSH sessions")
	sshMaxConnections := flag.Int("ssh-max-connections", defaultMaxSSHConns, "max concurrent SSH connections")
	gitIdleTimeout := flag.Duration("git-idle-timeout", defaultGitIdleTimeout, "cancel a git session that makes no progress for this long; 0 disables")

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
	if *gitMaxConcurrent < 1 {
		logError("'--git-max-concurrent' must be >= 1, got %d", *gitMaxConcurrent)
	}
	if *sshMaxConnections < 1 {
		logError("'--ssh-max-connections' must be >= 1, got %d", *sshMaxConnections)
	}
	if *gitIdleTimeout < 0 {
		logError("'--git-idle-timeout' must be >= 0 (0 disables), got %s", *gitIdleTimeout)
	}
	if !*enableSSHRead && !*enableSSHWrite {
		logError("at least one of '--enable-ssh-read' (fetch) or '--enable-ssh-write' (push) must be set; otherwise this server can serve nothing")
	}

	if _, err := exec.LookPath("git"); err != nil {
		logError("git binary not found on PATH (required for receive-pack/upload-pack): %v", err)
	}

	hostKeySigner, err := loadHostKey(*sshHostKey)
	if err != nil {
		logError("load host key: %v", err)
	}

	keys, keyCount, err := newKeyStore(*sshAuthorizedKeys)
	if err != nil {
		logError("load authorized_keys: %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	defer shutdownCancel()

	cfg := &sshServerConfig{
		shutdownCtx:    shutdownCtx,
		gitReposFolder: *gitReposFolder,
		enableSSHRead:  *enableSSHRead,
		enableSSHWrite: *enableSSHWrite,
		gitSem:         make(chan struct{}, *gitMaxConcurrent),
		gitIdleTimeout: *gitIdleTimeout,
	}

	sshConfig := &ssh.ServerConfig{
		PublicKeyCallback: publicKeyCallback(keys),
		ServerVersion:     "SSH-2.0-simple-git-server-ssh-read-write",
	}
	sshConfig.AddHostKey(hostKeySigner)

	listener, err := net.Listen("tcp", *sshOn)
	if err != nil {
		logError("listen on %s: %v", *sshOn, err)
	}

	logInfo("listen | ssh://%s", *sshOn)
	logInfo("gitReposFolder | %s", cfg.gitReposFolder)
	logInfo("auth   | authorized_keys=%s (%d key(s)); SIGHUP reloads", *sshAuthorizedKeys, keyCount)
	logInfo("access | fetch(upload-pack)=%v push(receive-pack)=%v", cfg.enableSSHRead, cfg.enableSSHWrite)
	logInfo("limits | max-conns=%d handshake=%s conn-idle=%s session-exec=%s git-idle=%s max-sessions-per-conn=%d max-git-procs=%d",
		*sshMaxConnections, sshHandshakeTimeout, cfg.connIdle(), cfg.sessionRequestDeadline(), *gitIdleTimeout, maxSessionsPerConn, *gitMaxConcurrent)

	errCh := make(chan error, 1)
	go acceptLoop(listener, sshConfig, cfg, *sshMaxConnections, errCh)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		select {
		case err := <-errCh:
			logInfo("err    | %v", err)
			_ = listener.Close()
			os.Exit(1)
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				n, err := keys.reload()
				if err != nil {
					logInfo("auth   | SIGHUP reload failed, keeping previous keys: %v", err)
				} else if n == 0 {
					logInfo("auth   | SIGHUP reloaded %s: 0 keys — ALL ACCESS REVOKED", *sshAuthorizedKeys)
				} else {
					logInfo("auth   | SIGHUP reloaded %s (%d key(s))", *sshAuthorizedKeys, n)
				}
				continue
			}
			logInfo("signal | %s", sig)
			_ = listener.Close()
			// Cancel every running git command so its process group - including
			// any hook it spawned - is killed rather than left orphaned, then
			// wait for them to actually go away before exiting.
			shutdownCancel()
			cfg.waitForGitCommands(gitShutdownGrace)
			return
		}
	}
}

// acceptLoop bounds the number of connections handled at once. Beyond the cap
// new connections are closed immediately rather than queued, so an attacker
// cannot pin unbounded file descriptors and goroutines pre-auth.
func acceptLoop(listener net.Listener, sshConfig *ssh.ServerConfig, cfg *sshServerConfig, maxConns int, errCh chan<- error) {
	connSem := make(chan struct{}, maxConns)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			errCh <- fmt.Errorf("accept: %w", err)
			return
		}
		select {
		case connSem <- struct{}{}:
			go func() {
				defer func() { <-connSem }()
				handleConn(conn, sshConfig, cfg)
			}()
		default:
			logInfo("ssh    | %s rejected: %d concurrent connections already in flight", conn.RemoteAddr(), maxConns)
			_ = conn.Close()
		}
	}
}

// connActivity tracks whether a connection has real work in flight. Only a
// running git command counts: an open-but-unused session channel must not keep
// the connection alive, or a client can hold slots forever without ever running
// anything.
type connActivity struct {
	active   atomic.Int64
	lastUsed atomic.Int64
}

func (a *connActivity) begin() {
	if a != nil {
		a.active.Add(1)
	}
}

func (a *connActivity) end() {
	if a != nil {
		a.active.Add(-1)
		a.lastUsed.Store(time.Now().UnixNano())
	}
}

type sshServerConfig struct {
	// shutdownCtx is cancelled when the server is stopping; every git command
	// derives its context from it so none outlive the process.
	shutdownCtx context.Context
	gitWG       sync.WaitGroup

	gitReposFolder string
	enableSSHRead  bool
	enableSSHWrite bool

	// gitSem caps concurrent git processes across every connection. Per-connection
	// session limits alone would still allow maxConns*maxSessionsPerConn children.
	gitSem chan struct{}

	// gitIdleTimeout cancels a git session that stops making progress.
	gitIdleTimeout time.Duration

	// connIdleTimeout closes a connection with no git command in flight;
	// sessionRequestTimeout closes a channel that never carries an exec.
	// Zero means the package default; they are fields rather than globals so
	// tests can shorten them without mutating shared state.
	connIdleTimeout       time.Duration
	sessionRequestTimeout time.Duration
}

// waitForGitCommands waits up to d for cancelled git commands to exit.
func (c *sshServerConfig) waitForGitCommands(d time.Duration) {
	done := make(chan struct{})
	go func() { c.gitWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		logInfo("ssh    | gave up waiting %s for git commands to exit", d)
	}
}

func (c *sshServerConfig) gitBase() context.Context {
	if c.shutdownCtx != nil {
		return c.shutdownCtx
	}
	return context.Background()
}

func (c *sshServerConfig) connIdle() time.Duration {
	if c.connIdleTimeout > 0 {
		return c.connIdleTimeout
	}
	return defaultConnIdleTimeout
}

func (c *sshServerConfig) sessionRequestDeadline() time.Duration {
	if c.sessionRequestTimeout > 0 {
		return c.sessionRequestTimeout
	}
	return defaultSessionRequestTimeout
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

	// Bound the handshake itself; a client that connects and says nothing must
	// not hold the socket open indefinitely.
	_ = nConn.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, sshConfig)
	if err != nil {
		logInfo("ssh    | %s handshake failed: %v", remote, err)
		return
	}
	// Clear it again: a legitimate clone or push may run far longer than the
	// handshake budget.
	_ = nConn.SetDeadline(time.Time{})
	defer sConn.Close()

	logInfo("ssh    | %s connected user=%s", remote, sConn.User())
	go ssh.DiscardRequests(reqs)

	// An authenticated connection that never opens a session would otherwise be
	// held forever, occupying one of the --ssh-max-connections slots; enough of
	// them lock every other client out. Close it once it has been idle.
	act := &connActivity{}
	act.lastUsed.Store(time.Now().UnixNano())
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		ticker := time.NewTicker(cfg.connIdle() / 4)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				if act.active.Load() > 0 {
					act.lastUsed.Store(time.Now().UnixNano())
					continue
				}
				if time.Since(time.Unix(0, act.lastUsed.Load())) >= cfg.connIdle() {
					logInfo("ssh    | %s idle %s with no git command, closing", remote, cfg.connIdle())
					_ = nConn.Close()
					return
				}
			}
		}
	}()

	sessions := make(chan struct{}, maxSessionsPerConn)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		select {
		case sessions <- struct{}{}:
		default:
			_ = newChannel.Reject(ssh.ResourceShortage, "too many concurrent sessions")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			<-sessions
			logInfo("ssh    | %s accept channel: %v", remote, err)
			continue
		}
		go func() {
			defer func() { <-sessions }()
			handleSession(channel, requests, cfg, remote, act)
		}()
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
