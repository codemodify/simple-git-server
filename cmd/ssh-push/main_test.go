package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
			got, err := parseGitSSHCommand(tc.cmd, true)
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
			_, err := parseGitSSHCommand(cmd, true)
			if err == nil {
				t.Fatalf("expected reject for %q", cmd)
			}
		})
	}
}

func TestParseGitSSHCommandUploadPackDisabled(t *testing.T) {
	_, err := parseGitSSHCommand("git-upload-pack 'x.git'", false)
	if err == nil {
		t.Fatal("expected upload-pack rejected when disabled")
	}
	if !strings.Contains(err.Error(), "upload-pack disabled") {
		t.Fatalf("unexpected error: %v", err)
	}

	got, err := parseGitSSHCommand("git-receive-pack 'x.git'", false)
	if err != nil {
		t.Fatalf("receive-pack should still work: %v", err)
	}
	if got.service != serviceReceivePack {
		t.Fatalf("service = %q", got.service)
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
