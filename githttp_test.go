package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSanitizeGitPathInfo(t *testing.T) {
	okCases := []struct {
		in, want string
	}{
		{"/cliflags.git/info/refs", "/cliflags.git/info/refs"},
		{"/cliflags.git/git-upload-pack", "/cliflags.git/git-upload-pack"},
		{"cliflags.git/info/refs", "/cliflags.git/info/refs"},
		{"/cliflags.git/info/../git-upload-pack", "/cliflags.git/git-upload-pack"},
		{"/my-repo.git", "/my-repo.git"},
		// a legitimate repo name may contain dots; only a ".." path segment escapes
		{"/weird..name.git/info/refs", "/weird..name.git/info/refs"},
	}
	for _, tc := range okCases {
		t.Run("ok:"+tc.in, func(t *testing.T) {
			got, err := sanitizeGitPathInfo(tc.in)
			if err != nil {
				t.Fatalf("sanitizeGitPathInfo(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("sanitizeGitPathInfo(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	badCases := []string{
		"",
		"/",
		"/../etc/passwd",
		"/cliflags.git/../../etc/passwd",
		"/foo/../../../etc/passwd",
		"/no-suffix/info/refs",
		"/.git/info/refs",
		"/..",
		"/cliflags/info/refs",
	}
	for _, in := range badCases {
		t.Run("bad:"+in, func(t *testing.T) {
			if _, err := sanitizeGitPathInfo(in); err == nil {
				t.Fatalf("sanitizeGitPathInfo(%q) expected error", in)
			}
		})
	}
}

func TestGoImportEscapesHTML(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, `x<script>y.git`), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := buildConfig(`evil"goframework.io`, root, true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, `/x<script>y`, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `<script>`) {
		t.Fatalf("unescaped script in body:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatalf("expected escaped script tags in body:\n%s", body)
	}
	if !strings.Contains(body, "&#34;") && !strings.Contains(body, "&quot;") {
		t.Fatalf("expected escaped quotes in body:\n%s", body)
	}
	if strings.Contains(body, `content="evil"goframework`) {
		t.Fatalf("unescaped quote broke meta attribute:\n%s", body)
	}
}

func TestBuildConfigSetsGitMaxConcurrentDefault(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.gitMaxConcurrent != 32 {
		t.Fatalf("gitMaxConcurrent = %d, want 32", cfg.gitMaxConcurrent)
	}
}

func TestIsReceivePackRequest(t *testing.T) {
	cases := []struct {
		pathInfo string
		rawQuery string
		want     bool
	}{
		{"/repo.git/git-receive-pack", "", true},
		{"/repo.git/info/refs", "service=git-receive-pack", true},
		{"/repo.git/git-upload-pack", "", false},
		{"/repo.git/info/refs", "service=git-upload-pack", false},
		{"/repo.git/info/refs", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.pathInfo+"?"+tc.rawQuery, func(t *testing.T) {
			u := &url.URL{Path: tc.pathInfo, RawQuery: tc.rawQuery}
			r := &http.Request{URL: u}
			got := isReceivePackRequest(r, tc.pathInfo)
			if got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestServeGitForbiddenWhenHTTPWriteDisabled(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	cfg.gitSem = make(chan struct{}, 1)
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/git/repo.git/info/refs?service=git-receive-pack", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "HTTP push") {
		t.Fatalf("body should mention HTTP push: %q", rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/git/repo.git/git-receive-pack", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("POST status = %d, want 403", rec2.Code)
	}
}

func TestGitBackendEnvPassesAuthorization(t *testing.T) {
	cfg := &serverConfig{gitReposFolder: "/var/git", httpGitReposPrefix: "/git/"}
	r := httptest.NewRequest(http.MethodGet, "/git/repo.git/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	r.RemoteAddr = "127.0.0.1:1"
	env := gitBackendEnv(cfg, "/repo.git/info/refs", r)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HTTP_AUTHORIZATION=Basic dXNlcjpwYXNz") {
		t.Fatalf("missing HTTP_AUTHORIZATION in env: %v", env)
	}
}

// git http-backend does not enforce containment itself, so a symlink under the
// repo root would otherwise export a repository from outside it.
func TestRepoWithinRootRejectsSymlinkEscape(t *testing.T) {
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

	if err := repoWithinRoot(root, "/link.git/info/refs"); err == nil {
		t.Fatal("symlinked outside repo must be rejected")
	}
	if err := repoWithinRoot(root, "/ok.git/info/refs"); err != nil {
		t.Fatalf("in-root repo must be accepted: %v", err)
	}
	// a repository that simply does not exist is not an escape; git returns 404
	if err := repoWithinRoot(root, "/missing.git/info/refs"); err != nil {
		t.Fatalf("nonexistent repo must not be treated as an escape: %v", err)
	}
}

// Containment must cover the whole served path, not just the "*.git" segment:
// the dumb HTTP protocol serves files far below the repository directory.
func TestRepoWithinRootRejectsDeepSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repos")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "stolen.pack"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(root, "ok.git", "objects", "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "pack-0000000000000000000000000000000000000000.pack"
	if err := os.Symlink(filepath.Join(outside, "stolen.pack"), filepath.Join(packDir, name)); err != nil {
		t.Fatal(err)
	}
	// a symlinked directory below the repo, with a non-existent leaf beneath it
	if err := os.Symlink(outside, filepath.Join(root, "ok.git", "objects", "info")); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"/ok.git/objects/pack/" + name,
		"/ok.git/objects/info/does-not-exist",
		"/ok.git/objects/info/packs",
	}
	for _, p := range escapes {
		t.Run("escape:"+p, func(t *testing.T) {
			if err := repoWithinRoot(root, p); err == nil {
				t.Fatalf("%s must be rejected as an escape", p)
			}
		})
	}

	ok := []string{"/ok.git/info/refs", "/ok.git/HEAD", "/ok.git/objects/aa/bbbb", "/missing.git/info/refs"}
	for _, p := range ok {
		t.Run("ok:"+p, func(t *testing.T) {
			if err := repoWithinRoot(root, p); err != nil {
				t.Fatalf("%s must be accepted: %v", p, err)
			}
		})
	}
}

// The CGI error path reads the stderr buffer; that must not race the os/exec
// copier goroutine still writing into it. Run with -race.
func TestRunGitCGIErrorPathNoStderrRace(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 300 ]; do echo \"stderr noise $i\" >&2; i=$((i+1)); done\nexit 3\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		enableHTTPWrite:    true,
		httpWriteBinary:    helper,
		gitMaxConcurrent:   8,
		gitIdleTimeout:     60 * time.Second,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	for i := 0; i < 8; i++ {
		resp, err := http.Post(srv.URL+"/git/repo.git/git-receive-pack",
			"application/x-git-receive-pack-request", strings.NewReader("0000"))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// git http-backend emits CGI headers before it consumes the request body, so a
// client that declares a body and then stops sending must still lose its
// concurrency slot. Tying the body timeout to the connection read deadline (as
// an earlier revision did) let such a client hold the slot until it disconnected.
func TestIncompletePostReleasesSlot(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	// headers first, then block forever reading stdin - exactly http-backend's shape
	// A grandchild inherits stdout and holds it open, exactly as git http-backend's
	// forked upload-pack/pack-objects do; killing only the direct child is not enough.
	script := "#!/bin/sh\nprintf 'Status: 200 OK\\r\\nContent-Type: application/x-git-receive-pack-result\\r\\n\\r\\n'\nsleep 300 &\ncat > /dev/null\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		enableHTTPWrite:    true,
		httpWriteBinary:    helper,
		gitMaxConcurrent:   1,
		gitIdleTimeout:     300 * time.Millisecond,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	pr, pw := io.Pipe() // never written to and never closed: the stalled upload
	defer pw.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/git/repo.git/git-receive-pack", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 100
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	probe := func() int {
		resp, err := http.Get(srv.URL + "/git/repo.git/info/refs?service=git-upload-pack")
		if err != nil {
			return -1
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	// the stalled request should be holding the only slot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && probe() != http.StatusServiceUnavailable {
		time.Sleep(20 * time.Millisecond)
	}

	// ...and must give it up once the body stalls past the idle window
	freed := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if probe() != http.StatusServiceUnavailable {
			freed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !freed {
		t.Fatal("stalled POST kept the concurrency slot past --git-idle-timeout")
	}
}

// The dumb HTTP protocol serves objects straight out of the object database, so
// anyone who knows an object ID can fetch it even when no ref reaches it any
// more. Smart HTTP must keep working.
func TestDumbProtocolObjectFetchIsRefused(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	repo := filepath.Join(root, "victim.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	hash := exec.Command("git", "--git-dir="+repo, "hash-object", "-w", "--stdin")
	hash.Stdin = strings.NewReader("unreachable secret\n")
	shaOut, err := hash.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	sha := strings.TrimSpace(string(shaOut))

	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		gitMaxConcurrent:   4,
		gitIdleTimeout:     30 * time.Second,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	objPath := "/git/victim.git/objects/" + sha[:2] + "/" + sha[2:]
	if code := get(objPath); code == http.StatusOK {
		t.Fatalf("unreachable object served over dumb HTTP (status %d)", code)
	}
	// smart HTTP is the supported path and must be unaffected
	if code := get("/git/victim.git/info/refs?service=git-upload-pack"); code != http.StatusOK {
		t.Fatalf("smart HTTP info/refs = %d, want 200", code)
	}
}

func TestGitBackendArgsDisableDumbProtocol(t *testing.T) {
	args := strings.Join(gitBackendArgs("false"), " ")
	for _, want := range []string{"http.receivepack=false", "http.getanyfile=false", "http-backend"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
}

// git's client gzips upload-pack requests over ~1 KiB. http-backend only
// decompresses when HTTP_CONTENT_ENCODING is set, so without this mapping any
// repo with more than a handful of refs fails to clone.
func TestGitBackendEnvMapsContentEncodingAndProtocol(t *testing.T) {
	cfg := &serverConfig{gitReposFolder: "/var/git", httpGitReposPrefix: "/git/"}
	r := httptest.NewRequest(http.MethodPost, "/git/repo.git/git-upload-pack", nil)
	r.Header.Set("Content-Encoding", "gzip")
	r.Header.Set("Git-Protocol", "version=2")
	joined := strings.Join(gitBackendEnv(cfg, "/repo.git/git-upload-pack", r), "\n")
	for _, want := range []string{"HTTP_CONTENT_ENCODING=gzip", "HTTP_GIT_PROTOCOL=version=2"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env missing %q:\n%s", want, joined)
		}
	}
}

// End-to-end: a gzip-compressed negotiation must produce a real pack, not an
// empty body from http-backend choking on gzip bytes as pkt-line headers.
func TestGzippedUploadPackRequestIsDecompressed(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(work, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "base")
	run(work, "clone", "-q", "--bare", work, filepath.Join(root, "many.git"))

	bare := filepath.Join(root, "many.git")
	head, err := exec.Command("git", "--git-dir="+bare, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.TrimSpace(string(head))
	// enough refs that a real client would gzip the negotiation
	var refs strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&refs, "create refs/heads/branch-with-a-long-name-%03d %s\n", i, sha)
	}
	upd := exec.Command("git", "--git-dir="+bare, "update-ref", "--stdin")
	upd.Stdin = strings.NewReader(refs.String())
	if out, err := upd.CombinedOutput(); err != nil {
		t.Fatalf("update-ref: %v %s", err, out)
	}

	cfg := &serverConfig{gitReposFolder: root, httpGitReposPrefix: "/git/", gitMaxConcurrent: 4, gitIdleTimeout: 30 * time.Second}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	plain := pkt("want "+sha+" multi_ack_detailed side-band-64k thin-pack ofs-delta agent=test\n") + "0000" + pkt("done\n")

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(plain))
	zw.Close()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/git/many.git/git-upload-pack", bytes.NewReader(gz.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(body) == 0 {
		t.Fatal("gzipped upload-pack request produced an empty response (Content-Encoding not forwarded)")
	}
	if !bytes.Contains(body, []byte("NAK")) && !bytes.Contains(body, []byte("PACK")) {
		t.Fatalf("response is not a git negotiation result (%d bytes): %q", len(body), body[:min(80, len(body))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// A request whose body is never sent must not hold the connection, even on
// routes that never reach the git concurrency limit.
func TestUnreadBodyDoesNotPinConnection(t *testing.T) {
	cfg := &serverConfig{
		gitReposFolder:     t.TempDir(),
		httpGitReposPrefix: "/git/",
		gitMaxConcurrent:   4,
		gitIdleTimeout:     400 * time.Millisecond,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	for _, path := range []string{"/", "/missing", "/git/nope"} {
		t.Run(path, func(t *testing.T) {
			c, err := net.DialTimeout("tcp", addr, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: x\r\nContent-Length: 20\r\n\r\n", path)
			c.SetReadDeadline(time.Now().Add(6 * time.Second))
			if _, err := io.ReadAll(c); err != nil {
				t.Fatalf("connection not released for %s: %v", path, err)
			}
		})
	}
}

// Documents the ACTUAL confidentiality boundary, so the readme is never again
// allowed to overclaim. git's upload-pack validates reachability for commits but
// not for blobs, so an unreferenced blob is still retrievable by exact SHA while
// it remains in the object database. If git ever tightens this, the test fails
// and the documentation should be revisited.
func TestUnreachableObjectDisclosureBoundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "pub.git")
	work := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run(work, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "public")
	run(work, "clone", "-q", "--bare", work, bare)

	hash := exec.Command("git", "--git-dir="+bare, "hash-object", "-w", "--stdin")
	hash.Stdin = strings.NewReader("removed secret\n")
	out, err := hash.Output()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.TrimSpace(string(out))

	cfg := &serverConfig{gitReposFolder: root, httpGitReposPrefix: "/git/", gitMaxConcurrent: 4, gitIdleTimeout: 30 * time.Second}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	// the dumb path is closed
	resp, err := http.Get(srv.URL + "/git/pub.git/objects/" + blob[:2] + "/" + blob[2:])
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("dumb object download must stay blocked, got %d", resp.StatusCode)
	}

	// ...but smart upload-pack still serves the blob. This is git behavior, and
	// the documentation says so; it is asserted here so the claim stays honest.
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	body := pkt("want "+blob+" multi_ack_detailed side-band-64k thin-pack ofs-delta agent=test\n") + "0000" + pkt("done\n")
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/git/pub.git/git-upload-pack", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || len(got) == 0 {
		t.Skipf("git %q did not serve the unreachable blob (status %d, %d bytes) — "+
			"upstream behavior may have changed; revisit the readme claim", "upload-pack", resp2.StatusCode, len(got))
	}
	t.Logf("documented boundary holds: unreachable blob IS served over smart HTTP (%d bytes)", len(got))
}

// A body that exceeds the size limit must end the request promptly and release
// the git slot. Previously the limit error was mistaken for normal EOF: the
// watchdog stood down and the backend kept running on truncated stdin.
func TestOversizedBodyReleasesSlotPromptly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "-q", filepath.Join(root, "repo.git")).CombinedOutput(); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		gitMaxConcurrent:   1,
		gitIdleTimeout:     200 * time.Millisecond,
		gitMaxRequestBody:  4, // tiny, so the limit trips without moving 100 MiB
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	body := strings.NewReader("0000" + strings.Repeat("x", 200))
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/git/repo.git/git-upload-pack", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request with an oversized body never terminated")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/git/repo.git/info/refs?service=git-upload-pack")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("git slot still held after the body limit was exceeded")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// When the limit trips before the backend has committed any headers, the client
// gets 413 rather than a misleading 502.
func TestOversizedBodyReturns413BeforeHeaders(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	// read stdin, never emit CGI headers
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ncat > /dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		enableHTTPWrite:    true,
		httpWriteBinary:    helper,
		gitMaxConcurrent:   2,
		gitIdleTimeout:     200 * time.Millisecond,
		gitMaxRequestBody:  4,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/git/repo.git/git-receive-pack",
		"application/x-git-receive-pack-request", strings.NewReader(strings.Repeat("x", 500)))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

// A backend that has consumed its input and then goes silent (a stuck hook) is
// as much a stall as one that never got input.
func TestSilentBackendAfterInputIsCancelled(t *testing.T) {
	dir := t.TempDir()
	helper := filepath.Join(dir, "helper")
	// consume stdin fully, emit headers, then go silent forever
	script := "#!/bin/sh\ncat > /dev/null\nprintf 'Status: 200 OK\\r\\nContent-Type: application/x-git-receive-pack-result\\r\\n\\r\\n'\nsleep 300\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{
		gitReposFolder:     root,
		httpGitReposPrefix: "/git/",
		enableHTTPWrite:    true,
		httpWriteBinary:    helper,
		gitMaxConcurrent:   1,
		gitIdleTimeout:     300 * time.Millisecond,
	}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	go func() {
		resp, err := http.Post(srv.URL+"/git/repo.git/git-receive-pack",
			"application/x-git-receive-pack-request", strings.NewReader("0000"))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	deadline := time.Now().Add(8 * time.Second)
	for {
		resp, err := http.Get(srv.URL + "/git/repo.git/info/refs?service=git-upload-pack")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				return // slot reclaimed
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("a backend silent after consuming stdin kept its slot")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Companion to TestUnreachableObjectDisclosureBoundary, for the protocol the
// server now negotiates. Protocol v2 lets a client fetch an unreachable COMMIT,
// which v0 refuses — so the documentation must not promise commits are safe.
// Asserted here so the claim cannot silently drift back.
func TestUnreachableCommitFetchableOverProtocolV2(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "pub.git")
	work := t.TempDir()
	env := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "HOME="+t.TempDir())
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(work, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "public")
	run(work, "clone", "-q", "--bare", work, bare)

	tree := run(bare, "--git-dir="+bare, "rev-parse", "HEAD^{tree}")
	mk := exec.Command("git", "--git-dir="+bare, "commit-tree", tree)
	mk.Env = env
	mk.Stdin = strings.NewReader("removed commit\n")
	out, err := mk.Output()
	if err != nil {
		t.Fatal(err)
	}
	dangling := strings.TrimSpace(string(out))
	if reach := run(bare, "--git-dir="+bare, "rev-list", "--all"); strings.Contains(reach, dangling) {
		t.Fatal("fixture is wrong: the commit is reachable")
	}

	cfg := &serverConfig{gitReposFolder: root, httpGitReposPrefix: "/git/", gitMaxConcurrent: 4, gitIdleTimeout: 30 * time.Second}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	fetch := func(version string) bool {
		dst := t.TempDir()
		run(dst, "init", "-q", dst)
		cmd := exec.Command("git", "-c", "protocol.version="+version, "fetch", "-q",
			srv.URL+"/git/pub.git", dangling)
		cmd.Dir, cmd.Env = dst, env
		if err := cmd.Run(); err != nil {
			return false
		}
		check := exec.Command("git", "cat-file", "-t", dangling)
		check.Dir, check.Env = dst, env
		got, err := check.Output()
		return err == nil && strings.TrimSpace(string(got)) == "commit"
	}

	if fetch("0") {
		t.Log("note: protocol v0 now also serves unreachable commits; revisit the readme")
	}
	if !fetch("2") {
		t.Skip("git no longer serves unreachable commits over v2 — upstream changed, revisit the readme claim")
	}
	t.Log("documented boundary holds: an unreachable COMMIT is fetchable over protocol v2")
}

// repoWithinRoot walks up one path component at a time, so an unbounded path
// costs quadratic time before any request is even rejected. A 690 KB URL once
// burned 35s of CPU; the path must be bounded before touching the filesystem.
func TestAbsurdPathsAreRejectedCheaply(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "x.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{gitReposFolder: root, httpGitReposPrefix: "/git/", gitMaxConcurrent: 4, gitIdleTimeout: 30 * time.Second}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	long := "/git/x.git/" + strings.Repeat("segment/", 50000)
	start := time.Now()
	resp, err := http.Get(srv.URL + long)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusRequestURITooLong {
		t.Fatalf("status = %d, want 414", resp.StatusCode)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("rejecting an absurd path took %s; it must be cheap", elapsed)
	}

	// a normal path still works
	ok, err := http.Get(srv.URL + "/git/x.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, ok.Body)
	ok.Body.Close()
	if ok.StatusCode == http.StatusRequestURITooLong {
		t.Fatal("a normal git path was rejected as too long")
	}
}

func TestSanitizeGitPathInfoRejectsDeepPaths(t *testing.T) {
	deep := "/repo.git/" + strings.Repeat("a/", maxGitPathSegments+5)
	if _, err := sanitizeGitPathInfo(deep); err == nil {
		t.Fatal("an excessively deep path must be rejected")
	}
	// the deepest real git URL is repo.git/objects/pack/pack-<sha>.pack
	if _, err := sanitizeGitPathInfo("/repo.git/objects/pack/pack-0000000000000000000000000000000000000000.pack"); err != nil {
		t.Fatalf("a real git path must be accepted: %v", err)
	}
}

func TestTruncateForLog(t *testing.T) {
	if got := truncateForLog("short"); got != "short" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("x", maxLoggedPathLen*4)
	got := truncateForLog(long)
	if len(got) >= len(long) {
		t.Fatalf("long path was not truncated (%d bytes)", len(got))
	}
	if !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("truncation not marked: %q", got[len(got)-20:])
	}
}

// Values too large to pass to the child (Linux caps one env string at
// MAX_ARG_STRLEN) must be reported as the client faults they are, not as a 502
// from the failed execve.
func TestOversizedCGIValuesReturn4xx(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &serverConfig{gitReposFolder: root, httpGitReposPrefix: "/git/", gitMaxConcurrent: 4, gitIdleTimeout: 30 * time.Second}
	srv := httptest.NewServer(newRootHandler(cfg))
	defer srv.Close()

	big := strings.Repeat("a", maxCGIEnvValue+1)
	base := srv.URL + "/git/repo.git/info/refs?service=git-upload-pack"

	resp, err := http.Get(base + "&x=" + big)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestURITooLong {
		t.Fatalf("oversized query: status = %d, want 414", resp.StatusCode)
	}

	for _, header := range []string{"Content-Type", "Authorization"} {
		req, err := http.NewRequest(http.MethodGet, base, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(header, big)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
			t.Fatalf("oversized %s: status = %d, want 431", header, resp.StatusCode)
		}
	}
}

// GIT_PROJECT_ROOT must stay set: it is what http-backend resolves PATH_INFO
// against, now that the PATH_TRANSLATED fallback is no longer sent.
func TestGitBackendEnvSetsProjectRootAndNotPathTranslated(t *testing.T) {
	cfg := &serverConfig{gitReposFolder: "/var/git", httpGitReposPrefix: "/git/"}
	r := httptest.NewRequest(http.MethodGet, "/git/repo.git/info/refs", nil)
	joined := strings.Join(gitBackendEnv(cfg, "/repo.git/info/refs", r), "\n")
	if !strings.Contains(joined, "GIT_PROJECT_ROOT=/var/git") {
		t.Fatalf("GIT_PROJECT_ROOT missing:\n%s", joined)
	}
	if strings.Contains(joined, "PATH_TRANSLATED=") {
		t.Fatalf("PATH_TRANSLATED is never consulted and should not be sent:\n%s", joined)
	}
}
