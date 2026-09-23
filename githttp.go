package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// defaultMaxGitRequestBody caps the request body. It bounds upload-pack
// negotiation AND the whole incoming pack on a push, so raise it with
// --git-max-request-body if you push repositories larger than this. Pack
// downloads are response-side and unaffected.
const defaultMaxGitRequestBody int64 = 100 << 20 // 100 MiB

// gitBackendArgs builds the http-backend argv.
//
// http.getanyfile=false disables git's dumb HTTP protocol, which serves files
// straight out of the object database. With it on, anyone who knows or can guess
// an object ID can fetch that object even when no ref reaches it any more - a
// force-pushed-away commit or a deleted secret stays downloadable until gc. Smart
// HTTP (info/refs?service=... plus the POST endpoints) is unaffected; only dumb
// clients predating git 1.6.6 need it.
func gitBackendArgs(receivePack string) []string {
	return []string{"-c", "http.receivepack=" + receivePack, "-c", "http.getanyfile=false", "http-backend"}
}

// serveGit handles git-over-HTTP under --http-git-repos-prefix.
// Read path: git -c http.receivepack=false -c http.getanyfile=false http-backend.
// Receive-pack: 403 unless --enable-http-write, then exec simple-git-server-http-write.
func serveGit(w http.ResponseWriter, r *http.Request, cfg *serverConfig) {
	// Check before taking a concurrency slot: this is a cheap, client-side fault.
	if code, msg := checkCGIEnvLimits(r); code != 0 {
		http.Error(w, msg, code)
		return
	}

	select {
	case cfg.gitSem <- struct{}{}:
		defer func() { <-cfg.gitSem }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many concurrent git requests", http.StatusServiceUnavailable)
		return
	}

	prefixTrim := strings.TrimSuffix(cfg.httpGitReposPrefix, "/")
	pathInfo := strings.TrimPrefix(r.URL.Path, prefixTrim)
	cleaned, err := sanitizeGitPathInfo(pathInfo)
	if err != nil {
		http.Error(w, "bad git path: "+err.Error(), http.StatusBadRequest)
		return
	}

	// git http-backend resolves GIT_PROJECT_ROOT+PATH_INFO itself and does not
	// enforce containment, so a symlink planted under the root would export an
	// outside repository. Answer 404 rather than confirming it exists.
	if err := repoWithinRoot(cfg.gitReposFolder, cleaned); err != nil {
		logInfo("git-http | %v (path=%q)", err, cleaned)
		http.NotFound(w, r)
		return
	}

	// Clear any write deadline this handler sets before returning, so it cannot
	// outlive the request and fire against the next one on a keep-alive connection.
	if cfg.gitIdleTimeout > 0 {
		rc := http.NewResponseController(w)
		defer func() {
			_ = rc.SetReadDeadline(time.Time{})
			_ = rc.SetWriteDeadline(time.Time{})
		}()
	}

	r.Body = http.MaxBytesReader(w, r.Body, cfg.maxRequestBody())

	if isReceivePackRequest(r, cleaned) {
		if !cfg.enableHTTPWrite {
			http.Error(w, "HTTP push (git-receive-pack) is disabled; enable with --enable-http-write (private network only, no auth) or use SSH", http.StatusForbidden)
			return
		}
		serveGitHTTPWrite(w, r, cfg, cleaned)
		return
	}

	runGitCGIRequest(w, r, cfg, func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "git", gitBackendArgs("false")...)
		cmd.Env = gitBackendEnv(cfg, cleaned, r)
		return cmd
	})
}

func serveGitHTTPWrite(w http.ResponseWriter, r *http.Request, cfg *serverConfig, pathInfo string) {
	binary, err := resolveHTTPWriteBinary(cfg.httpWriteBinary)
	if err != nil {
		logInfo("http-write | %v", err)
		http.Error(w, "HTTP push helper unavailable", http.StatusBadGateway)
		return
	}
	runGitCGIRequest(w, r, cfg, func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, binary)
		cmd.Env = gitBackendEnv(cfg, pathInfo, r)
		return cmd
	})
}

// runGitCGIRequest wires a CGI child to the request with body-stall cancellation.
//
// The request body must be bounded INDEPENDENTLY of the connection's read
// deadline. git http-backend emits CGI headers before it has consumed the body,
// so any scheme that ties the two together either cuts off long downloads or
// (as a previous revision did) hands a stalled uploader the concurrency slot for
// as long as it holds the socket. Progress on the body is tracked directly and a
// stall cancels the child's context.
func runGitCGIRequest(w http.ResponseWriter, r *http.Request, cfg *serverConfig, build func(context.Context) *exec.Cmd) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cfg.gitBegin()
	defer cfg.gitEnd()

	prog := &requestProgress{}
	prog.mark()
	stop := make(chan struct{})
	defer close(stop)
	go watchStall(prog, cfg.gitIdleTimeout, http.NewResponseController(w), cancel, stop)

	// Shutdown must reach in-flight requests: closing the listener does not
	// cancel handlers, and their git process groups would otherwise be orphaned.
	if sc := cfg.shutdown(); sc != nil {
		go func() {
			select {
			case <-stop:
			case <-sc.Done():
				cancel()
			}
		}()
	}

	cmd := build(ctx)
	killProcessGroupOnCancel(cmd)
	cmd.WaitDelay = gitWaitDelay
	cmd.Stdin = &progressReader{r: r.Body, p: prog}
	runGitCGI(w, cmd, cfg.gitIdleTimeout, prog)
}

func runGitCGI(w http.ResponseWriter, cmd *exec.Cmd, idle time.Duration, prog *requestProgress) {
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
	if err := writeCGIResponse(w, stdout, &headersWritten, idle, prog); err != nil {
		// Wait first: it reaps the stderr copier goroutine, so reading the
		// buffer afterwards does not race with it.
		waitErr := cmd.Wait()
		logInfo("git-http | cgi: %v (wait: %v) stderr=%s", err, waitErr, strings.TrimSpace(stderr.String()))
		if !headersWritten {
			// A body that blew the size limit is the client's fault, not the
			// backend's; say so instead of reporting a bad gateway.
			var tooLarge *http.MaxBytesError
			if bodyErr := prog.readErr(); bodyErr != nil && errors.As(bodyErr, &tooLarge) {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "bad git http-backend response", http.StatusBadGateway)
			}
		}
		return
	}
	if err := cmd.Wait(); err != nil {
		logInfo("git-http | wait: %v stderr=%s", err, strings.TrimSpace(stderr.String()))
	}
}

func writeCGIResponse(w http.ResponseWriter, r io.Reader, headersWritten *bool, idle time.Duration, prog *requestProgress) error {
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
	_, err := io.Copy(streamWriter(w, idle, prog), br)
	return err
}

const (
	gitWaitDelay = 5 * time.Second

	// gitShutdownGrace bounds how long shutdown waits for cancelled git children
	// (and their hooks) to actually die before giving up.
	gitShutdownGrace = 3 * time.Second
)

// killProcessGroupOnCancel makes cancellation reach the whole git process tree.
//
// git http-backend forks git upload-pack, which in turn forks git pack-objects,
// and those grandchildren inherit the stdout pipe. Killing only the direct child
// leaves that pipe open, so the handler stays blocked copying a response nobody
// will ever write - which is how a stalled request kept its concurrency slot
// even after the idle watchdog fired and killed http-backend itself.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// requestProgress records when a request last advanced in EITHER direction -
// bytes read from the body or written to the response - so the watchdog covers
// the whole exchange rather than only the input phase.
type requestProgress struct {
	last     atomic.Int64 // unix nanoseconds
	bodyDone atomic.Bool  // body reached EOF
	mu       sync.Mutex
	err      error // first non-EOF body read error, e.g. the size limit
}

func (p *requestProgress) mark() { p.last.Store(time.Now().UnixNano()) }

func (p *requestProgress) idleFor() time.Duration {
	return time.Since(time.Unix(0, p.last.Load()))
}

func (p *requestProgress) failRead(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

func (p *requestProgress) readErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

type progressReader struct {
	r io.Reader
	p *requestProgress
}

func (t *progressReader) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if n > 0 {
		t.p.mark()
	}
	switch {
	case err == nil:
	case errors.Is(err, io.EOF):
		t.p.bodyDone.Store(true) // normal completion
	default:
		// The body hit the size limit or the client went away. Recording it lets
		// the handler answer 413, and stops a dead body being mistaken for a
		// healthy finished one.
		t.p.failRead(err)
	}
	return n, err
}

// watchStall cancels the child when the request stops making progress in either
// direction, or as soon as the body fails outright. It runs for the whole
// request, so a backend that consumed its input and then went silent - a stuck
// pre-receive hook, say - is caught too. Timing is progress-based, so transfers
// of any size are safe.
func watchStall(p *requestProgress, idle time.Duration, rc *http.ResponseController, cancel context.CancelFunc, stop <-chan struct{}) {
	if idle <= 0 {
		return
	}
	tick := idle / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if p.readErr() == nil && p.idleFor() < idle {
				continue
			}
			// Killing the child is not enough on its own: cmd.Wait then blocks on
			// the os/exec goroutine still copying the request body, so the handler
			// keeps its slot until the client disconnects. Expire the read deadline
			// to fail that pending read - only while the body is still open, and
			// only on the failure path, so a healthy transfer is never touched.
			if rc != nil && !p.bodyDone.Load() {
				_ = rc.SetReadDeadline(time.Now())
			}
			cancel()
			return
		}
	}
}

// streamWriter flushes each chunk of CGI output so sideband progress reaches the
// client while the pack is still being generated, and bounds how long a client
// that has stopped reading may stall the response.
func streamWriter(w http.ResponseWriter, idle time.Duration, prog *requestProgress) io.Writer {
	return &deadlineWriter{w: w, rc: http.NewResponseController(w), d: idle, p: prog}
}

type deadlineWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
	d  time.Duration
	p  *requestProgress
}

func (t *deadlineWriter) Write(p []byte) (int, error) {
	if t.d > 0 {
		_ = t.rc.SetWriteDeadline(time.Now().Add(t.d))
	}
	n, err := t.w.Write(p)
	if n > 0 && t.p != nil {
		t.p.mark() // output is progress too, even after stdin is done
	}
	if err == nil {
		_ = t.rc.Flush()
	}
	return n, err
}

func isReceivePackRequest(r *http.Request, pathInfo string) bool {
	if strings.HasSuffix(pathInfo, "/git-receive-pack") {
		return true
	}
	return r.URL.Query().Get("service") == "git-receive-pack"
}

// maxCGIEnvValue bounds each request value forwarded into the child environment.
// Linux caps a single environment string at MAX_ARG_STRLEN (128 KiB); past that
// execve fails with E2BIG, which surfaced as a 502 even though the fault is
// entirely client-side.
const maxCGIEnvValue = 32 << 10

// checkCGIEnvLimits reports a 4xx status for request parts too large to pass to
// the child, or 0 when everything fits.
func checkCGIEnvLimits(r *http.Request) (int, string) {
	if len(r.URL.RawQuery) > maxCGIEnvValue {
		return http.StatusRequestURITooLong, "query string too long"
	}
	for _, name := range []string{"Content-Type", "Authorization"} {
		if len(r.Header.Get(name)) > maxCGIEnvValue {
			return http.StatusRequestHeaderFieldsTooLarge, name + " header too long"
		}
	}
	return 0, ""
}

// maxGitPathSegments bounds how many components repoWithinRoot may have to walk.
// The deepest real git URL is repo.git/objects/pack/pack-<sha>.pack - five.
const maxGitPathSegments = 32

// sanitizeGitPathInfo cleans PATH_INFO and requires a "*.git" repo segment.
func sanitizeGitPathInfo(pathInfo string) (string, error) {
	if pathInfo == "" || pathInfo == "/" {
		return "", fmt.Errorf("empty path")
	}
	// Bound the work before any filesystem call: repoWithinRoot walks up one
	// component at a time, so an unbounded path costs quadratic time.
	if n := strings.Count(pathInfo, "/"); n > maxGitPathSegments {
		return "", fmt.Errorf("path too deep")
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
	if hasDotDotSegment(cleaned) {
		return "", fmt.Errorf("path escape")
	}
	rel := strings.TrimPrefix(cleaned, "/")
	seg, _, _ := strings.Cut(rel, "/")
	if seg == "" || !strings.HasSuffix(seg, ".git") || seg == ".git" {
		return "", fmt.Errorf("repo segment must end in .git")
	}
	return cleaned, nil
}

// repoWithinRoot verifies that the path git http-backend will serve resolves,
// after symlinks, to a location inside root.
//
// It must consider the WHOLE path, not just the "*.git" segment: the dumb HTTP
// protocol serves files well below the repository directory (objects/pack/*,
// objects/<xx>/<38>, objects/info/*, HEAD), so a symlink planted at any of those
// depths would otherwise read a file from outside the root.
//
// A path that does not exist is not an escape; git http-backend produces the
// 404 for it.
func repoWithinRoot(root, pathInfo string) error {
	rel := strings.TrimPrefix(pathInfo, "/")
	if rel == "" {
		return fmt.Errorf("empty repo segment")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolving git-repos-folder: %w", err)
	}

	// Walk up to the deepest component that exists. EvalSymlinks on it resolves
	// every symlink along the way, so checking that one result covers every
	// intermediate directory too.
	probe := filepath.Join(resolvedRoot, filepath.FromSlash(rel))
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			return containedIn(resolvedRoot, resolved)
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("resolving repository path: %w", err)
		}
		parent := filepath.Dir(probe)
		if parent == probe || len(parent) < len(resolvedRoot) {
			return nil
		}
		probe = parent
	}
}

// containedIn reports whether target is root or lies beneath it. Both must
// already have symlinks resolved.
func containedIn(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("repository escapes git-repos-folder")
	}
	return nil
}

// hasDotDotSegment reports whether any segment of p is exactly "..".
// A strings.Contains(p, "..") check would also reject legitimate repository
// names such as "weird..name.git".
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func gitBackendEnv(cfg *serverConfig, pathInfo string, r *http.Request) []string {
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		pathEnv = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + pathEnv,
		"LANG=C",
		// git http-backend resolves GIT_PROJECT_ROOT+PATH_INFO. PATH_TRANSLATED is
		// only its fallback for when GIT_PROJECT_ROOT is unset, so it is not sent:
		// a second path-derived variable that never participates in resolution just
		// invites someone to trust it.
		"GIT_PROJECT_ROOT=" + cfg.gitReposFolder,
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=" + pathInfo,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"SCRIPT_NAME=" + strings.TrimSuffix(cfg.httpGitReposPrefix, "/"),
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
	// git's client gzips upload-pack requests once they exceed ~1 KiB (any repo
	// with more than a handful of refs). http-backend only decompresses when it
	// sees HTTP_CONTENT_ENCODING; without it the gzip bytes are parsed as pkt-line
	// headers and the clone dies with "the remote end hung up unexpectedly".
	if enc := r.Header.Get("Content-Encoding"); enc != "" {
		env = append(env, "HTTP_CONTENT_ENCODING="+enc)
	}
	// Protocol v2 negotiation is requested through this header; without it
	// http-backend silently falls back to v0.
	if proto := r.Header.Get("Git-Protocol"); proto != "" {
		env = append(env, "HTTP_GIT_PROTOCOL="+proto)
	}
	return env
}

// limitedBuffer keeps at most n bytes of stderr for logs. It is written by the
// os/exec copier goroutine and read by the request goroutine; cmd.WaitDelay can
// let Wait return while that copier is still running, so it locks.
type limitedBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
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
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}
