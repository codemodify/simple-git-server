package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// reposWith creates a temp repo root containing the given bare repo directories
// and returns its path. go-import now answers only for repos that exist.
func reposWith(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n+".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestProjectFromPath(t *testing.T) {
	testCases := []struct {
		path string
		want string
	}{
		{"/", ""},
		{"", ""},
		{"/cliflags", "cliflags"},
		{"/cliflags/", "cliflags"},
		{"/cliflags/pkg/sub", "cliflags"},
		{"/simple-git-server", "simple-git-server"},
	}
	for _, tc := range testCases {
		t.Run(tc.path, func(t *testing.T) {
			got := projectFromPath(tc.path)
			if got != tc.want {
				t.Fatalf("projectFromPath(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestGoImportMetaSelfHosted(t *testing.T) {
	cfg := &serverConfig{
		goImportDomain:     "goframework.io",
		httpGitReposPrefix: "/git/",
		goImportGitURL:     "https://{domain}/git/{repo}.git",
	}
	got := goImportMeta(cfg, "cliflags")
	want := "goframework.io/cliflags git https://goframework.io/git/cliflags.git"
	if got != want {
		t.Fatalf("goImportMeta = %q, want %q", got, want)
	}
}

func TestRepoURLForDefaultAndOverride(t *testing.T) {
	cfg := &serverConfig{
		goImportDomain:     "goframework.io",
		httpGitReposPrefix: "/git/",
		goImportGitURL:     "https://{domain}/git/{repo}.git",
	}
	if got := repoURLFor(cfg, "cliflags"); got != "https://goframework.io/git/cliflags.git" {
		t.Fatalf("default-style template: got %q", got)
	}

	cfg.goImportGitURL = "git://other.example/{repo}.git"
	if got := repoURLFor(cfg, "x"); got != "git://other.example/x.git" {
		t.Fatalf("override template: got %q", got)
	}
}

func TestBuildConfigRequiresDomainForGoImport(t *testing.T) {
	_, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err == nil {
		t.Fatal("expected error when domain missing with go-import enabled")
	}
}

func TestBuildConfigNormalizesPrefix(t *testing.T) {
	cfg, err := buildConfig("goframework.io", "/var/git", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "git", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.httpGitReposPrefix != "/git/" {
		t.Fatalf("prefix = %q, want /git/", cfg.httpGitReposPrefix)
	}
	if cfg.goImportGitURL != "https://{domain}/git/{repo}.git" {
		t.Fatalf("goImportGitURL = %q", cfg.goImportGitURL)
	}
}

func TestBuildConfigRejectsBothHTTPDisabled(t *testing.T) {
	_, err := buildConfig("", ".", false, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err == nil {
		t.Fatal("expected error when both http and https disabled")
	}
}

func TestBuildConfigAcceptsHTTPSOnly(t *testing.T) {
	cfg, err := buildConfig("", ".", false, "127.0.0.1:64180", true, "127.0.0.1:64143", "c.pem", "k.pem", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.enableHTTPSRead || cfg.enableHTTPRead {
		t.Fatalf("want https-only: http=%v https=%v", cfg.enableHTTPRead, cfg.enableHTTPSRead)
	}
}

func TestGoImportHandlerReturnsMeta(t *testing.T) {
	cfg, err := buildConfig("goframework.io", reposWith(t, "cliflags"), true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/cliflags?go-get=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wantMeta := `content="goframework.io/cliflags git https://goframework.io/git/cliflags.git"`
	if !strings.Contains(body, wantMeta) {
		t.Fatalf("body missing meta %q; got:\n%s", wantMeta, body)
	}
	if !strings.Contains(body, `name="go-import"`) {
		t.Fatalf("body missing go-import name; got:\n%s", body)
	}
}

func TestGoImportHandlerNestedPath(t *testing.T) {
	cfg, err := buildConfig("goframework.io", reposWith(t, "cliflags"), true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/cliflags/sub/pkg", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "goframework.io/cliflags git https://goframework.io/git/cliflags.git") {
		t.Fatalf("unexpected body:\n%s", rec.Body.String())
	}
}

func TestPulse(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, pulsePath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), banner) {
		t.Fatalf("body missing banner: %q", rec.Body.String())
	}

	// "/" is no longer special: with go-import off it is just a 404.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("GET / = %d, want 404 now that the route moved to %s", rec2.Code, pulsePath)
	}
}

func TestNonGitPaths404WhenGoImportOff(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/cliflags?go-get=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when go-import off", rec.Code)
	}
}

// go-import must not answer for a repository that does not exist, otherwise the
// handler is a catch-all that returns 200 for every path on the domain.
func TestGoImportUnknownProject404s(t *testing.T) {
	cfg, err := buildConfig("goframework.io", reposWith(t, "cliflags"), true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	for _, path := range []string{"/does-not-exist", "/favicon.ico", "/.env", "/../etc"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: status = %d, want 404; body=%q", path, rec.Code, rec.Body.String())
			}
		})
	}

	// the repo that does exist still resolves
	req := httptest.NewRequest(http.MethodGet, "/cliflags", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("existing repo: status = %d, want 200", rec.Code)
	}
}

func TestBuildConfigSSHReadRequiresKeys(t *testing.T) {
	ssh := defaultSSHReadWriteOptions()
	ssh.enable = true
	_, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", ssh, defaultHTTPWriteOptions())
	if err == nil {
		t.Fatal("expected error when enable-ssh-read without keys")
	}
}

// go-import must not confirm the existence of a directory outside the repo root.
func TestGoImportDoesNotLeakOutsideRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repos")
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "secret.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "real.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.git"), filepath.Join(root, "leak.git")); err != nil {
		t.Fatal(err)
	}

	cfg, err := buildConfig("goframework.io", root, true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/leak?go-get=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("symlinked-outside project: status = %d, want 404", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/real?go-get=1", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("in-root project: status = %d, want 200", rec2.Code)
	}
}

// "/" would route every request to git, hiding the status page and go-import.
// "/" mounts the git endpoints at the root so clone URLs are host/repo.git.
func TestBuildConfigAcceptsRootPrefix(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatalf("--http-git-repos-prefix / must be accepted: %v", err)
	}
	if cfg.httpGitReposPrefix != "/" {
		t.Fatalf("prefix = %q, want /", cfg.httpGitReposPrefix)
	}
	// a normal prefix still normalizes
	cfg, err = buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "git", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil || cfg.httpGitReposPrefix != "/git/" {
		t.Fatalf("prefix = %q, err = %v", cfg.httpGitReposPrefix, err)
	}
}

// Root-mounted, the three surfaces must not shadow one another: *.git paths are
// git, everything else falls through to /pulse and go-import.
func TestRootMountedRoutingCoexists(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "repo1.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := buildConfig("domain.com", root, true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/", "", defaultSSHReadWriteOptions(), defaultHTTPWriteOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	get := func(path string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	if code, _ := get("/pulse"); code != http.StatusOK {
		t.Fatalf("/pulse = %d, want 200 (git must not shadow it)", code)
	}
	code, body := get("/repo1?go-get=1")
	if code != http.StatusOK {
		t.Fatalf("go-import = %d, want 200", code)
	}
	// with the endpoints at the root the advertised clone URL loses the /git/ hop
	if !strings.Contains(body, `content="domain.com/repo1 git https://domain.com/repo1.git"`) {
		t.Fatalf("unexpected go-import tag:\n%s", body)
	}
	// and a *.git path is routed to git rather than go-import
	if _, b := get("/repo1.git/info/refs?service=git-upload-pack"); strings.Contains(b, "go-import") {
		t.Fatal("a *.git path was handled by go-import instead of git")
	}
}

func TestIsGitRequest(t *testing.T) {
	rootMounted := &serverConfig{httpGitReposPrefix: "/"}
	prefixed := &serverConfig{httpGitReposPrefix: "/git/"}
	cases := []struct {
		cfg  *serverConfig
		path string
		want bool
	}{
		{rootMounted, "/repo1.git/info/refs", true},
		{rootMounted, "/a/b.git", false}, // only the FIRST segment counts
		{rootMounted, "/repo1", false},
		{rootMounted, "/pulse", false},
		{rootMounted, "/", false},
		{rootMounted, "/.git/info/refs", false},
		{prefixed, "/git/repo1.git/info/refs", true},
		{prefixed, "/git/anything", true}, // prefix alone settles it
		{prefixed, "/repo1.git", false},
		{prefixed, "/pulse", false},
	}
	for _, tc := range cases {
		if got := isGitRequest(tc.cfg, tc.path); got != tc.want {
			t.Errorf("isGitRequest(%q, prefix=%q) = %v, want %v", tc.path, tc.cfg.httpGitReposPrefix, got, tc.want)
		}
	}
}

// The in-flight git counter must tolerate registration racing shutdown, which is
// exactly the ordering a sync.WaitGroup forbids.
func TestWaitForGitCommandsToleratesLateRegistration(t *testing.T) {
	cfg := &serverConfig{}
	start := time.Now()
	cfg.waitForGitCommands(2 * time.Second) // nothing running: returns at once
	if el := time.Since(start); el > time.Second {
		t.Fatalf("waited %s with nothing running", el)
	}

	// register concurrently with the wait, then finish
	cfg.gitBegin()
	go func() {
		time.Sleep(150 * time.Millisecond)
		cfg.gitEnd()
	}()
	start = time.Now()
	cfg.waitForGitCommands(3 * time.Second)
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("returned after %s without waiting for the in-flight command", el)
	}
	if n := cfg.gitActive.Load(); n != 0 {
		t.Fatalf("counter = %d, want 0", n)
	}

	// a command that never finishes must not block past the grace period
	cfg.gitBegin()
	start = time.Now()
	cfg.waitForGitCommands(300 * time.Millisecond)
	if el := time.Since(start); el < 250*time.Millisecond || el > 3*time.Second {
		t.Fatalf("grace period not honored: %s", el)
	}
	cfg.gitEnd()
}
