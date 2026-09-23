package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestParseGitSSHCommandAccepts(t *testing.T) {
	cases := []struct {
		cmd     string
		service gitService
		repo    string
	}{
		{"git-receive-pack 'cliflags.git'", serviceReceivePack, "cliflags.git"},
		{"git-receive-pack cliflags.git", serviceReceivePack, "cliflags.git"},
		{"git receive-pack 'cliflags.git'", serviceReceivePack, "cliflags.git"},
		{"git-upload-pack 'cliflags.git'", serviceUploadPack, "cliflags.git"},
		{"git upload-pack \"cliflags.git\"", serviceUploadPack, "cliflags.git"},
		{"git-receive-pack 'subdir/cliflags.git'", serviceReceivePack, "subdir/cliflags.git"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			got, err := parseGitSSHCommand(tc.cmd, true, true)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.service != tc.service {
				t.Fatalf("service = %q, want %q", got.service, tc.service)
			}
			if got.repo != tc.repo {
				t.Fatalf("repo = %q, want %q", got.repo, tc.repo)
			}
		})
	}
}

func TestParseGitSSHCommandRejects(t *testing.T) {
	bad := []string{
		"",
		"bash",
		"/bin/sh",
		"git",
		"git status",
		"git-receive-pack",
		"git-receive-pack a b",
		"git foo-pack 'x.git'",
		"rm -rf /",
		"git-receive-pack 'x.git'; id",
	}
	for _, cmd := range bad {
		t.Run(cmd, func(t *testing.T) {
			_, err := parseGitSSHCommand(cmd, true, true)
			if err == nil {
				t.Fatalf("expected reject for %q", cmd)
			}
		})
	}
}

// upload-pack is the READ side and receive-pack is the WRITE side. Each must be
// gated by its own capability: --enable-ssh-read must never permit a push, and
// --enable-ssh-write must never be required for a fetch.
func TestParseGitSSHCommandCapabilityGating(t *testing.T) {
	cases := []struct {
		name       string
		cmd        string
		allowFetch bool
		allowPush  bool
		wantErr    string // "" means the command must be accepted
	}{
		{"fetch allowed by read", "git-upload-pack 'x.git'", true, false, ""},
		{"push allowed by write", "git-receive-pack 'x.git'", false, true, ""},
		{"fetch blocked without read", "git-upload-pack 'x.git'", false, true, "--enable-ssh-read is off"},
		{"push blocked without write", "git-receive-pack 'x.git'", true, false, "--enable-ssh-write is off"},
		{"read-only server rejects push", "git-receive-pack 'x.git'", true, false, "--enable-ssh-write is off"},
		{"both off rejects fetch", "git-upload-pack 'x.git'", false, false, "--enable-ssh-read is off"},
		{"both off rejects push", "git-receive-pack 'x.git'", false, false, "--enable-ssh-write is off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGitSSHCommand(tc.cmd, tc.allowFetch, tc.allowPush)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected %q to be accepted, got: %v", tc.cmd, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected (got service %q)", tc.cmd, got.service)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestResolveRepoPathAllowsDotsInRepoName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "weird..name.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveRepoPath(root, "weird..name.git")
	if err != nil {
		t.Fatalf("a repo name containing '..' must not be rejected: %v", err)
	}
	if !strings.HasSuffix(got, "weird..name.git") {
		t.Fatalf("got %q", got)
	}
}

func TestParseAuthorizedKeysRefusesUnenforceableOptions(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	// A key restricted to git-shell must not be silently promoted to full access.
	restricted := `command="git-shell -c \"$SSH_ORIGINAL_COMMAND\"",no-pty ` + pubLine
	keys, err := parseAuthorizedKeys([]byte(restricted))
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("key carrying command= must be refused, got %d key(s)", len(keys))
	}
}

func TestResolveRepoPathSafety(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cliflags.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ok.git"), 0o755); err != nil {
		t.Fatal(err)
	}

	okCases := []struct {
		in   string
		want string // suffix
	}{
		{"cliflags.git", "cliflags.git"},
		{"cliflags", "cliflags.git"},
		{"./ok.git", "ok.git"},
		{"/ok.git", "ok.git"},
	}
	for _, tc := range okCases {
		t.Run("ok:"+tc.in, func(t *testing.T) {
			got, err := resolveRepoPath(root, tc.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.HasSuffix(got, tc.want) {
				t.Fatalf("got %q, want suffix %q", got, tc.want)
			}
			if !strings.HasPrefix(got, root) {
				t.Fatalf("path %q not under root %q", got, root)
			}
		})
	}

	bad := []string{
		"..",
		"../etc/passwd",
		"foo/../../etc",
		"cliflags.git/..",
		"..git",
		"missing.git",
	}
	for _, in := range bad {
		t.Run("bad:"+in, func(t *testing.T) {
			_, err := resolveRepoPath(root, in)
			if err == nil {
				t.Fatalf("expected reject for %q", in)
			}
		})
	}

	// path-only checks without Stat (escape / abs)
	_, err := resolveRepoPathOpts(root, "../escape.git", false)
	if err == nil {
		t.Fatal("expected reject for ../escape.git")
	}
}

func TestParseAuthorizedKeys(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubLine := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	raw := []byte("# comment\n\n" + pubLine + "not-a-key\n")
	keys, err := parseAuthorizedKeys(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want 1", len(keys))
	}
	if _, ok := keys[string(signer.PublicKey().Marshal())]; !ok {
		t.Fatal("expected generated public key to be present")
	}
}

func TestLoadHostKeyRoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "host_key")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(f, block); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	signer, err := loadHostKey(path)
	if err != nil {
		t.Fatalf("loadHostKey: %v", err)
	}
	if signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("type = %s", signer.PublicKey().Type())
	}
}

// A symlink planted under the repo root must not expose a repository outside it.
// The lexical checks alone accept it, so containment is re-checked after
// symlink resolution.
func TestResolveRepoPathRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repos")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "secret.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ok.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.git"), filepath.Join(root, "link.git")); err != nil {
		t.Fatal(err)
	}

	if _, err := resolveRepoPath(root, "link.git"); err == nil {
		t.Fatal("symlink pointing outside the repo root must be rejected")
	}
	// a real repo inside the root still resolves
	if _, err := resolveRepoPath(root, "ok.git"); err != nil {
		t.Fatalf("in-root repo must still resolve: %v", err)
	}
}

// Emptying authorized_keys and reloading is a revocation, not a failure: keeping
// the previous keys would leave revoked access alive. An unreadable file is
// different and must keep the previous set.
func TestKeyStoreReloadEmptyFileRevokes(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}

	store, n, err := newKeyStore(path)
	if err != nil || n != 1 {
		t.Fatalf("newKeyStore: n=%d err=%v", n, err)
	}
	if !store.authorized(signer.PublicKey()) {
		t.Fatal("key should be authorized initially")
	}

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.reload()
	if err != nil {
		t.Fatalf("reload of an empty file must succeed as a revocation, got: %v", err)
	}
	if got != 0 {
		t.Fatalf("reload count = %d, want 0", got)
	}
	if store.authorized(signer.PublicKey()) {
		t.Fatal("key must be revoked after the file is emptied and reloaded")
	}

	// an unreadable file keeps whatever is currently loaded
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.reload(); err == nil {
		t.Fatal("reload of a missing file must report an error")
	}
}

// Startup is different from a reload: a file with no usable keys is a
// configuration error, not a revocation.
func TestNewKeyStoreRejectsEmptyAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(path, []byte("# only a comment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newKeyStore(path); err == nil {
		t.Fatal("startup with no usable keys must fail")
	}
}

// authorized_keys options are default-deny: only options that disable a
// capability this server never offers are safe to accept.
func TestAuthorizedKeysOptionAllowlist(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))

	accepted := []string{
		"no-pty",
		"restrict",
		"no-pty,no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-user-rc",
	}
	for _, opts := range accepted {
		t.Run("accept:"+opts, func(t *testing.T) {
			keys, err := parseAuthorizedKeys([]byte(opts + " " + pub))
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 {
				t.Fatalf("option(s) %q only disable capabilities this server never offers; want the key accepted, got %d", opts, len(keys))
			}
		})
	}

	refused := []string{
		`command="git-shell -c \"$SSH_ORIGINAL_COMMAND\""`,
		`from="10.0.0.0/8"`,
		"cert-authority",
		"verify-required",
		`environment="PATH=/evil"`,
		`expiry-time="20200101"`,
		`permitopen="host:1"`,
		`principals="alice"`,
	}
	for _, opts := range refused {
		t.Run("refuse:"+opts, func(t *testing.T) {
			keys, err := parseAuthorizedKeys([]byte(opts + " " + pub))
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 0 {
				t.Fatalf("option %q carries access semantics this server cannot enforce; want the key refused", opts)
			}
		})
	}
}

// A git session that stops moving bytes must be cancelled; one that keeps making
// progress must not be.
func TestWatchGitStall(t *testing.T) {
	t.Run("cancels on stall", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &progress{}
		p.mark()
		stop := make(chan struct{})
		defer close(stop)
		go watchGitStall(p, 150*time.Millisecond, nil, cancel, stop, "test")
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("stalled session was never cancelled")
		}
	})

	t.Run("leaves a progressing session alone", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p := &progress{}
		p.mark()
		stop := make(chan struct{})
		defer close(stop)
		go watchGitStall(p, 300*time.Millisecond, nil, cancel, stop, "test")
		for i := 0; i < 20; i++ { // keep making progress for ~1s
			time.Sleep(50 * time.Millisecond)
			p.mark()
		}
		if ctx.Err() != nil {
			t.Fatal("a session that kept making progress was cancelled")
		}
	})
}

// A session channel that is accepted and never carries an exec must be closed,
// otherwise it holds a session slot and keeps the connection permanently
// "active" so the connection idle watchdog never reaps it either.
func TestSessionWithoutExecIsClosed(t *testing.T) {
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, cliPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cliSigner, err := ssh.NewSignerFromKey(cliPriv)
	if err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(keyPath, ssh.MarshalAuthorizedKey(cliSigner.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := newKeyStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	srvCfg := &ssh.ServerConfig{PublicKeyCallback: publicKeyCallback(store)}
	srvCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := &sshServerConfig{
		gitReposFolder:        t.TempDir(),
		enableSSHRead:         true,
		gitSem:                make(chan struct{}, 4),
		sessionRequestTimeout: 250 * time.Millisecond,
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handleConn(conn, srvCfg, cfg)
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cliSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)

	// no exec is ever sent; the server must close the channel on its own
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 1)
		_, err := ch.Read(buf)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session channel with no exec request was never closed")
	}
}

// An open-but-unused session channel must not count as connection activity, or a
// client can hold a connection slot forever by cycling channels without ever
// running a git command.
func TestIdleSessionChannelsDoNotKeepConnectionAlive(t *testing.T) {
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, cliPriv, _ := ed25519.GenerateKey(rand.Reader)
	cliSigner, err := ssh.NewSignerFromKey(cliPriv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(keyPath, ssh.MarshalAuthorizedKey(cliSigner.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := newKeyStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := &ssh.ServerConfig{PublicKeyCallback: publicKeyCallback(store)}
	srvCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := &sshServerConfig{
		gitReposFolder:        t.TempDir(),
		enableSSHRead:         true,
		gitSem:                make(chan struct{}, 4),
		sessionRequestTimeout: 150 * time.Millisecond,
		connIdleTimeout:       400 * time.Millisecond,
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handleConn(conn, srvCfg, cfg)
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cliSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for i := 0; i < 4; i++ { // hold several idle channels open
		ch, reqs, err := client.OpenChannel("session", nil)
		if err != nil {
			break
		}
		go ssh.DiscardRequests(reqs)
		_ = ch
	}

	closed := make(chan struct{})
	go func() { client.Wait(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(8 * time.Second):
		t.Fatal("connection held open indefinitely by idle session channels")
	}
}

// The mirror of the previous test: a connection with a git command actually
// running must NOT be reaped, however short the connection idle timeout is.
func TestRunningGitCommandKeepsConnectionAlive(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-q", filepath.Join(root, "proj.git")).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	_, cliPriv, _ := ed25519.GenerateKey(rand.Reader)
	cliSigner, err := ssh.NewSignerFromKey(cliPriv)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(keyPath, ssh.MarshalAuthorizedKey(cliSigner.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _, err := newKeyStore(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	srvCfg := &ssh.ServerConfig{PublicKeyCallback: publicKeyCallback(store)}
	srvCfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cfg := &sshServerConfig{
		gitReposFolder:  root,
		enableSSHRead:   true,
		gitSem:          make(chan struct{}, 4),
		connIdleTimeout: 300 * time.Millisecond, // far shorter than the work below
		gitIdleTimeout:  30 * time.Second,
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		handleConn(conn, srvCfg, cfg)
	}()

	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(cliSigner)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	// upload-pack starts and then waits for our want-lines: work is in flight
	payload := ssh.Marshal(struct{ Command string }{"git-upload-pack 'proj.git'"})
	if _, err := ch.SendRequest("exec", true, payload); err != nil {
		t.Fatal(err)
	}

	closed := make(chan struct{})
	go func() { client.Wait(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("connection with a running git command was reaped by the idle watchdog")
	case <-time.After(2 * time.Second): // ~6 idle windows
	}
}

// git commands must derive their context from the server shutdown context, so
// stopping the server kills them (and their hooks) instead of orphaning them.
func TestShutdownContextGovernsGitCommands(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := &sshServerConfig{shutdownCtx: ctx}
	if cfg.gitBase() != ctx {
		t.Fatal("git commands must derive from the shutdown context")
	}
	cancel()
	select {
	case <-cfg.gitBase().Done():
	default:
		t.Fatal("cancelling shutdown must cancel the git command base context")
	}

	// a config without a shutdown context still yields a usable base
	if (&sshServerConfig{}).gitBase() == nil {
		t.Fatal("gitBase must never return nil")
	}
}

func TestWaitForGitCommands(t *testing.T) {
	cfg := &sshServerConfig{}
	start := time.Now()
	cfg.waitForGitCommands(2 * time.Second) // nothing running: returns at once
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s with no commands running", elapsed)
	}

	cfg.gitWG.Add(1) // simulate a command that refuses to die
	start = time.Now()
	cfg.waitForGitCommands(300 * time.Millisecond)
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("returned after %s, expected to wait out the grace period", elapsed)
	}
	cfg.gitWG.Done()
}
