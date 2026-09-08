package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		goImportDomain: "goframework.io",
		gitURLPrefix:   "/git/",
		goImportGitURL: "https://{domain}/git/{repo}.git",
	}
	got := goImportMeta(cfg, "cliflags")
	want := "goframework.io/cliflags git https://goframework.io/git/cliflags.git"
	if got != want {
		t.Fatalf("goImportMeta = %q, want %q", got, want)
	}
}

func TestRepoURLForDefaultAndOverride(t *testing.T) {
	cfg := &serverConfig{
		goImportDomain: "goframework.io",
		gitURLPrefix:   "/git/",
		goImportGitURL: "https://{domain}/git/{repo}.git",
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
	_, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
	if err == nil {
		t.Fatal("expected error when domain missing with go-import enabled")
	}
}

func TestBuildConfigNormalizesPrefix(t *testing.T) {
	cfg, err := buildConfig("goframework.io", "/var/git", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "git", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.gitURLPrefix != "/git/" {
		t.Fatalf("prefix = %q, want /git/", cfg.gitURLPrefix)
	}
	if cfg.goImportGitURL != "https://{domain}/git/{repo}.git" {
		t.Fatalf("goImportGitURL = %q", cfg.goImportGitURL)
	}
}

func TestBuildConfigRejectsBothHTTPDisabled(t *testing.T) {
	_, err := buildConfig("", ".", false, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
	if err == nil {
		t.Fatal("expected error when both http and https disabled")
	}
}

func TestBuildConfigAcceptsHTTPSOnly(t *testing.T) {
	cfg, err := buildConfig("", ".", false, "127.0.0.1:64180", true, "127.0.0.1:64143", "c.pem", "k.pem", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.enableHTTPSRead || cfg.enableHTTPRead {
		t.Fatalf("want https-only: http=%v https=%v", cfg.enableHTTPRead, cfg.enableHTTPSRead)
	}
}

func TestGoImportHandlerReturnsMeta(t *testing.T) {
	cfg, err := buildConfig("goframework.io", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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
	cfg, err := buildConfig("goframework.io", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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

func TestRootHealth(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
	if err != nil {
		t.Fatal(err)
	}
	handler := newRootHandler(cfg)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), banner) {
		t.Fatalf("body missing banner: %q", rec.Body.String())
	}
}

func TestNonGitPaths404WhenGoImportOff(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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

func TestBuildConfigSSHReadRequiresKeys(t *testing.T) {
	ssh := defaultSSHPushOptions()
	ssh.enable = true
	_, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", ssh, defaultHTTPPushOptions())
	if err == nil {
		t.Fatal("expected error when enable-ssh-read without keys")
	}
}
