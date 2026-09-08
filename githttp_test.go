package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
	cfg, err := buildConfig(`evil"goframework.io`, ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", true, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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

func TestServeGitForbiddenWhenHTTPPushDisabled(t *testing.T) {
	cfg, err := buildConfig("", ".", true, "127.0.0.1:64180", false, "127.0.0.1:64143", "", "", false, "/git/", "", defaultSSHPushOptions(), defaultHTTPPushOptions())
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
	cfg := &serverConfig{gitReposFolder: "/var/git", gitURLPrefix: "/git/"}
	r := httptest.NewRequest(http.MethodGet, "/git/repo.git/info/refs?service=git-upload-pack", nil)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	r.RemoteAddr = "127.0.0.1:1"
	env := gitBackendEnv(cfg, "/repo.git/info/refs", r)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HTTP_AUTHORIZATION=Basic dXNlcjpwYXNz") {
		t.Fatalf("missing HTTP_AUTHORIZATION in env: %v", env)
	}
}
