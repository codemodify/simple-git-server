package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

const maxGitRequestBody = 100 << 20 // 100 MiB — upload-pack negotiation; pack download is response-side

// serveGit handles git-over-HTTP under --git-url-prefix.
// Read path: git -c http.receivepack=false http-backend.
// Receive-pack: 403 unless --enable-http-write, then exec simple-git-server-http-push.
func serveGit(w http.ResponseWriter, r *http.Request, cfg *serverConfig) {
	select {
	case cfg.gitSem <- struct{}{}:
		defer func() { <-cfg.gitSem }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many concurrent git requests", http.StatusServiceUnavailable)
		return
	}

	prefixTrim := strings.TrimSuffix(cfg.gitURLPrefix, "/")
	pathInfo := strings.TrimPrefix(r.URL.Path, prefixTrim)
	cleaned, err := sanitizeGitPathInfo(pathInfo)
	if err != nil {
		http.Error(w, "bad git path: "+err.Error(), http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxGitRequestBody)

	if isReceivePackRequest(r, cleaned) {
		if !cfg.enableHTTPWrite {
			http.Error(w, "HTTP push (git-receive-pack) is disabled; enable with --enable-http-write (private network only, no auth) or use SSH", http.StatusForbidden)
			return
		}
		serveGitHTTPPush(w, r, cfg, cleaned)
		return
	}

	env := gitBackendEnv(cfg, cleaned, r)
	cmd := exec.Command("git", "-c", "http.receivepack=false", "http-backend")
	cmd.Env = env
	cmd.Stdin = r.Body
	runGitCGI(w, cmd)
}

func serveGitHTTPPush(w http.ResponseWriter, r *http.Request, cfg *serverConfig, pathInfo string) {
	binary, err := resolveHTTPPushBinary(cfg.httpPushBinary)
	if err != nil {
		logInfo("http-push | %v", err)
		http.Error(w, "HTTP push helper unavailable", http.StatusBadGateway)
		return
	}
	env := gitBackendEnv(cfg, pathInfo, r)
	cmd := exec.Command(binary)
	cmd.Env = env
	cmd.Stdin = r.Body
	runGitCGI(w, cmd)
}

func runGitCGI(w http.ResponseWriter, cmd *exec.Cmd) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "git http-backend failed", http.StatusBadGateway)
		return
	}
	var stderr limitedBuffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		logInfo("git-http | start: %v", err)
		http.Error(w, "git http-backend failed", http.StatusBadGateway)
		return
	}

	headersWritten := false
	if err := writeCGIResponse(w, stdout, &headersWritten); err != nil {
		logInfo("git-http | cgi: %v stderr=%s", err, strings.TrimSpace(stderr.String()))
		_ = cmd.Wait()
		if !headersWritten {
			http.Error(w, "bad git http-backend response", http.StatusBadGateway)
		}
		return
	}
	if err := cmd.Wait(); err != nil {
		logInfo("git-http | wait: %v stderr=%s", err, strings.TrimSpace(stderr.String()))
	}
}

func writeCGIResponse(w http.ResponseWriter, r io.Reader, headersWritten *bool) error {
	br := bufio.NewReader(r)
	status := http.StatusOK

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading cgi headers: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return fmt.Errorf("malformed cgi header: %q", line)
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if strings.EqualFold(key, "Status") {
			fields := strings.Fields(value)
			if len(fields) > 0 {
				if code, err := strconv.Atoi(fields[0]); err == nil {
					status = code
				}
			}
			continue
		}
		w.Header().Add(key, value)
	}

	w.WriteHeader(status)
	if headersWritten != nil {
		*headersWritten = true
	}
	_, err := io.Copy(w, br)
	return err
}

func isReceivePackRequest(r *http.Request, pathInfo string) bool {
	if strings.HasSuffix(pathInfo, "/git-receive-pack") {
		return true
	}
	return r.URL.Query().Get("service") == "git-receive-pack"
}

// sanitizeGitPathInfo cleans PATH_INFO and requires a "*.git" repo segment.
func sanitizeGitPathInfo(pathInfo string) (string, error) {
	if pathInfo == "" || pathInfo == "/" {
		return "", fmt.Errorf("empty path")
	}
	if !strings.HasPrefix(pathInfo, "/") {
		pathInfo = "/" + pathInfo
	}
	cleaned := path.Clean(pathInfo)
	if cleaned == "." || cleaned == "/" {
		return "", fmt.Errorf("empty path")
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("path escape")
	}
	rel := strings.TrimPrefix(cleaned, "/")
	seg, _, _ := strings.Cut(rel, "/")
	if seg == "" || !strings.HasSuffix(seg, ".git") || seg == ".git" {
		return "", fmt.Errorf("repo segment must end in .git")
	}
	return cleaned, nil
}

func gitBackendEnv(cfg *serverConfig, pathInfo string, r *http.Request) []string {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + pathEnv,
		"LANG=C",
		"GIT_PROJECT_ROOT=" + cfg.gitReposFolder,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=" + pathInfo,
		"PATH_TRANSLATED=" + cfg.gitReposFolder + pathInfo,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"SCRIPT_NAME=" + strings.TrimSuffix(cfg.gitURLPrefix, "/"),
		"REMOTE_ADDR=" + r.RemoteAddr,
		"REMOTE_USER=",
		"SERVER_PROTOCOL=" + r.Proto,
	}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	}
	if r.ContentLength >= 0 {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		env = append(env, "HTTP_AUTHORIZATION="+auth)
	}
	return env
}

// limitedBuffer keeps at most n bytes of stderr for logs.
type limitedBuffer struct {
	buf []byte
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.n == 0 {
		l.n = 64 << 10
	}
	if len(l.buf) < l.n {
		need := l.n - len(l.buf)
		if len(p) < need {
			need = len(p)
		}
		l.buf = append(l.buf, p[:need]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	return string(l.buf)
}
