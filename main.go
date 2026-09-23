package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

var version = "dev"

const (
	banner = "codemodify/simple-git-server"

	// pulsePath answers with the banner and version. It touches neither the
	// filesystem nor a subprocess, so it is cheap enough to poll.
	pulsePath      = "/pulse"
	defaultHTTPOn  = "127.0.0.1:64180"
	defaultHTTPSOn = "127.0.0.1:64143"
	defaultRepos   = "/home/git"
	defaultPrefix  = "/"

	defaultGitMaxConcurrent = 32

	// maxURLPathLen bounds the request path before any handler touches the
	// filesystem or the log. Real git URLs are ~60 bytes; this is far more than
	// anything legitimate needs.
	maxURLPathLen = 4096

	// maxLoggedPathLen keeps one absurd request from writing an absurd log line.
	maxLoggedPathLen = 256
	// defaultGitIdleTimeout bounds how long a git request may make no progress.
	// Without it a client can declare a large Content-Length, trickle a byte and
	// pin a concurrency slot plus a git process indefinitely.
	defaultGitIdleTimeout = 60 * time.Second
)

func init() {
	log.SetFlags(0)
}

func main() {
	showVersion := flag.Bool("version", false, "print version")

	gitReposFolder := flag.String("git-repos-folder", defaultRepos, "root folder for git repos")
	httpGitReposPrefix := flag.String("http-git-repos-prefix", defaultPrefix, "URL path prefix the git endpoints are served under, on HTTP and HTTPS alike. The default \"/\" gives clone URLs like https://host/repo.git; set e.g. /git/ to namespace them on a shared domain. SSH is unaffected")

	enableHTTPRead := flag.Bool("enable-http-read", true, "enable HTTP listener (read)")
	httpOn := flag.String("http-on", defaultHTTPOn, "HTTP bind address")

	enableHTTPSRead := flag.Bool("enable-https-read", false, "enable HTTPS listener (read)")
	httpsOn := flag.String("https-on", defaultHTTPSOn, "HTTPS bind address")
	httpsCert := flag.String("https-cert", "", "HTTPS Cert")
	httpsKey := flag.String("https-key", "", "HTTPS Key")

	// HTTP push: per-request simple-git-server-http-write helper (CGI), gated separately from read-only http-backend.
	// No auth — private networks only; never expose --enable-http-write on the public internet.
	enableHTTPWrite := flag.Bool("enable-http-write", false, "allow unauthenticated git-receive-pack (push) over HTTP via simple-git-server-http-write (private network only)")
	httpWriteBinary := flag.String("http-write-binary", defaultHTTPWriteBinary, "simple-git-server-http-write binary name or absolute path")

	// SSH push: supervise long-lived simple-git-server-ssh-read-write child (not per-request like http-backend).
	enableSSHRead := flag.Bool("enable-ssh-read", false, "enable supervised SSH listener via simple-git-server-ssh-read-write; allows git-upload-pack (clone/fetch)")
	sshOn := flag.String("ssh-on", defaultSSHOn, "SSH bind address (passed to ssh-read-write)")
	sshHostKey := flag.String("ssh-host-key", "", "path to SSH host private key (required if --enable-ssh-read)")
	sshAuthorizedKeys := flag.String("ssh-authorized-keys", "", "path to authorized_keys (required if --enable-ssh-read)")
	sshReadWriteBinary := flag.String("ssh-read-write-binary", defaultSSHReadWriteBinary, "simple-git-server-ssh-read-write binary name or absolute path")
	enableSSHWrite := flag.Bool("enable-ssh-write", false, "allow git-receive-pack (push) over SSH; requires --enable-ssh-read, passed through to supervised simple-git-server-ssh-read-write")

	enableGoImport := flag.Bool("enable-go-import", false, "serve go-import meta pages")
	goImportDomain := flag.String("go-import-domain", "", "domain for go-import; required when --enable-go-import")
	goImportGitURL := flag.String("go-import-git-url", "", "override template for repo URL; default https://{domain}{httpGitReposPrefix}{repo}.git")
	gitMaxConcurrent := flag.Int("git-max-concurrent", 32, "max concurrent git http-backend processes")
	gitIdleTimeout := flag.Duration("git-idle-timeout", defaultGitIdleTimeout, "per-request idle timeout for git request/response body transfer; 0 disables")
	gitMaxRequestBody := flag.Int64("git-max-request-body", defaultMaxGitRequestBody, "max request body in bytes; bounds upload-pack negotiation and the whole pack on a push")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	sshReadWriteOpts := sshReadWriteOptions{
		enable:             *enableSSHRead,
		sshOn:              *sshOn,
		hostKey:            *sshHostKey,
		authorizedKeys:     *sshAuthorizedKeys,
		sshReadWriteBinary: *sshReadWriteBinary,
		enableSSHWrite:     *enableSSHWrite,
	}
	httpWriteOpts := httpWriteOptions{
		enable:          *enableHTTPWrite,
		httpWriteBinary: *httpWriteBinary,
	}

	cfg, err := buildConfig(*goImportDomain, *gitReposFolder, *enableHTTPRead, *httpOn, *enableHTTPSRead, *httpsOn, *httpsCert, *httpsKey, *enableGoImport, *httpGitReposPrefix, *goImportGitURL, sshReadWriteOpts, httpWriteOpts)
	if err != nil {
		logError("%v", err)
	}
	if *gitMaxConcurrent < 1 {
		logError("'--git-max-concurrent' must be >= 1, got %d", *gitMaxConcurrent)
	}
	if *gitIdleTimeout < 0 {
		logError("'--git-idle-timeout' must be >= 0 (0 disables), got %s", *gitIdleTimeout)
	}
	if *gitMaxRequestBody < 1 {
		logError("'--git-max-request-body' must be >= 1, got %d", *gitMaxRequestBody)
	}
	cfg.gitMaxConcurrent = *gitMaxConcurrent
	cfg.gitIdleTimeout = *gitIdleTimeout
	cfg.gitMaxRequestBody = *gitMaxRequestBody

	if _, err := exec.LookPath("git"); err != nil {
		logError("git binary not found on PATH (required for git server): %v", err)
	}
	if cfg.enableHTTPWrite {
		if _, err := resolveHTTPWriteBinary(cfg.httpWriteBinary); err != nil {
			logError("%v", err)
		}
	}

	appCtx, appCancel := context.WithCancel(context.Background())
	defer appCancel()
	cfg.appCtx = appCtx

	handler := accessLog(newRootHandler(cfg))

	if cfg.enableHTTPRead {
		logInfo("listen | http://%s", cfg.httpOn)
	}
	if cfg.enableHTTPSRead {
		logInfo("listen | https://%s", cfg.httpsOn)
	}
	logInfo("gitReposFolder | %s", cfg.gitReposFolder)
	if cfg.enableGoImport {
		logInfo("go-import | domain=%s prefix=%s", cfg.goImportDomain, cfg.httpGitReposPrefix)
	}
	if cfg.enableHTTPWrite {
		logInfo("git    | git server on %s (HTTP push via %s; NO AUTH — private network only; GIT_HTTP_EXPORT_ALL=1)", cfg.httpGitReposPrefix, cfg.httpWriteBinary)
	} else {
		logInfo("git    | git server on %s (read-only HTTP; receive-pack off; GIT_HTTP_EXPORT_ALL=1)", cfg.httpGitReposPrefix)
	}

	const (
		readHeaderTimeout = 10 * time.Second
		idleTimeout       = 120 * time.Second
		shutdownTimeout   = 5 * time.Second
	)

	var ssh *sshChild
	if cfg.enableSSHRead {
		binary, err := resolveSSHReadWriteBinary(cfg.sshReadWriteBinary)
		if err != nil {
			logError("%v", err)
		}
		ssh, err = startSSHReadWrite(cfg, binary)
		if err != nil {
			logError("start simple-git-server-ssh-read-write: %v", err)
		}
		logInfo("ssh    | supervised simple-git-server-ssh-read-write pid=%d listen=%s binary=%s (long-lived child, not per-push)", ssh.cmd.Process.Pid, cfg.sshOn, binary)
		logInfo("ssh    | access: fetch(upload-pack)=%v push(receive-pack)=%v", cfg.enableSSHRead, cfg.enableSSHWrite)
	}

	var servers []*http.Server
	errCh := make(chan error, 2)

	if cfg.enableHTTPRead {
		srv := &http.Server{
			Addr:              cfg.httpOn,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
		}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("http server on %s: %w", srv.Addr, err)
			}
		}()
	}

	if cfg.enableHTTPSRead {
		srv := &http.Server{
			Addr:              cfg.httpsOn,
			Handler:           handler,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			// Pin the floor rather than inheriting whatever the toolchain
			// currently defaults to.
			TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}
		servers = append(servers, srv)
		go func() {
			if err := srv.ListenAndServeTLS(cfg.httpsCert, cfg.httpsKey); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("https server on %s: %w", srv.Addr, err)
			}
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var serverErr error
	var sshExited bool
	if ssh != nil {
		select {
		case serverErr = <-errCh:
			logInfo("err    | %v", serverErr)
		case sig := <-sigCh:
			logInfo("signal | %s", sig)
		case err := <-ssh.done:
			sshExited = true
			serverErr = fmt.Errorf("simple-git-server-ssh-read-write exited unexpectedly: %w", err)
			logInfo("err    | %v", serverErr)
		}
	} else {
		select {
		case serverErr = <-errCh:
			logInfo("err    | %v", serverErr)
		case sig := <-sigCh:
			logInfo("signal | %s", sig)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	for _, srv := range servers {
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logInfo("err    | shutdown: %v", err)
		}
	}
	cancel()

	// Graceful shutdown is over. Cancel whatever is still running so each git
	// process group - hooks included - is killed rather than orphaned, then wait
	// for them to actually go away.
	appCancel()
	cfg.waitForGitCommands(gitShutdownGrace)

	if ssh != nil && !sshExited {
		logInfo("ssh    | stopping supervised simple-git-server-ssh-read-write")
		ssh.stop(sshChildStopTimeout)
	}

	if serverErr != nil {
		os.Exit(1)
	}
}

type serverConfig struct {
	// appCtx is cancelled when the server stops. Every git child derives from it,
	// so shutdown reaches in-flight requests instead of orphaning their process
	// groups: closing the listener does not cancel running handlers.
	appCtx context.Context

	// gitActive counts git children in flight. A sync.WaitGroup would be wrong
	// here: Add runs inside a handler, Wait runs after graceful shutdown may have
	// timed out with handlers still running, and Add-when-zero racing Wait is
	// documented misuse. A counter has no such ordering rule.
	gitActive atomic.Int64

	enableHTTPRead bool
	httpOn         string

	enableHTTPSRead bool
	httpsOn         string
	httpsCert       string
	httpsKey        string

	gitReposFolder     string
	httpGitReposPrefix string

	enableGoImport bool
	goImportDomain string
	goImportGitURL string // template with {domain} (=goImportDomain) and {repo} placeholders

	gitMaxConcurrent  int           // max concurrent git http-backend processes
	gitIdleTimeout    time.Duration // per-request idle timeout on body transfer; 0 disables
	gitMaxRequestBody int64         // max request body bytes; 0 means the default
	gitSem            chan struct{} // semaphore; sized in newRootHandler

	// SSH push supervision (long-lived simple-git-server-ssh-read-write child; not per-request).
	enableSSHRead      bool
	sshOn              string
	sshHostKey         string
	sshAuthorizedKeys  string
	sshReadWriteBinary string
	enableSSHWrite     bool

	// HTTP push via per-request simple-git-server-http-write helper (CGI). No auth — private network only.
	enableHTTPWrite bool
	httpWriteBinary string
}

func buildConfig(goImportDomain, gitReposFolder string, enableHTTPRead bool, httpOn string, enableHTTPSRead bool, httpsOn, httpsCert, httpsKey string, enableGoImport bool, httpGitReposPrefix, goImportGitURL string, ssh sshReadWriteOptions, httpWrite httpWriteOptions) (*serverConfig, error) {
	if !enableHTTPRead && !enableHTTPSRead && !ssh.enable {
		return nil, errors.New("nothing to serve: enable at least one of '--enable-http-read', '--enable-https-read' or '--enable-ssh-read'")
	}
	if enableHTTPSRead {
		if httpsCert == "" {
			return nil, errors.New("'--https-cert' is required when '--enable-https-read' is set")
		}
		if httpsKey == "" {
			return nil, errors.New("'--https-key' is required when '--enable-https-read' is set")
		}
	}
	if enableGoImport && strings.TrimSpace(goImportDomain) == "" {
		return nil, errors.New("'--go-import-domain' is required when '--enable-go-import' is set")
	}

	if httpWrite.enable && !enableHTTPRead && !enableHTTPSRead {
		return nil, errors.New("'--enable-http-write' needs an HTTP or HTTPS listener: enable '--enable-http-read' or '--enable-https-read'")
	}
	if ssh.enableSSHWrite && !ssh.enable {
		return nil, errors.New("'--enable-ssh-write' requires '--enable-ssh-read' (there is no SSH listener to push to otherwise)")
	}
	if ssh.enable {
		if strings.TrimSpace(ssh.hostKey) == "" {
			return nil, errors.New("'--ssh-host-key' is required when '--enable-ssh-read' is set")
		}
		if strings.TrimSpace(ssh.authorizedKeys) == "" {
			return nil, errors.New("'--ssh-authorized-keys' is required when '--enable-ssh-read' is set")
		}
		if _, err := os.Stat(ssh.hostKey); err != nil {
			return nil, fmt.Errorf("'--ssh-host-key' file: %w", err)
		}
		if _, err := os.Stat(ssh.authorizedKeys); err != nil {
			return nil, fmt.Errorf("'--ssh-authorized-keys' file: %w", err)
		}
	}

	prefix := httpGitReposPrefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix = prefix + "/"
	}

	template := goImportGitURL
	if template == "" {
		template = "https://{domain}" + prefix + "{repo}.git"
	}

	sshOn := ssh.sshOn
	if sshOn == "" {
		sshOn = defaultSSHOn
	}
	binary := ssh.sshReadWriteBinary
	if binary == "" {
		binary = defaultSSHReadWriteBinary
	}
	httpBinary := httpWrite.httpWriteBinary
	if httpBinary == "" {
		httpBinary = defaultHTTPWriteBinary
	}

	return &serverConfig{
		enableHTTPRead: enableHTTPRead,
		httpOn:         httpOn,

		enableHTTPSRead: enableHTTPSRead,
		httpsOn:         httpsOn,
		httpsCert:       httpsCert,
		httpsKey:        httpsKey,

		gitReposFolder:     gitReposFolder,
		httpGitReposPrefix: prefix,

		enableGoImport: enableGoImport,
		goImportDomain: strings.TrimSpace(goImportDomain),
		goImportGitURL: template,

		gitMaxConcurrent: defaultGitMaxConcurrent,
		gitIdleTimeout:   defaultGitIdleTimeout,

		enableSSHRead:      ssh.enable,
		sshOn:              sshOn,
		sshHostKey:         ssh.hostKey,
		sshAuthorizedKeys:  ssh.authorizedKeys,
		sshReadWriteBinary: binary,
		enableSSHWrite:     ssh.enableSSHWrite,

		enableHTTPWrite: httpWrite.enable,
		httpWriteBinary: httpBinary,
	}, nil
}

func (c *serverConfig) shutdown() context.Context { return c.appCtx }

func (c *serverConfig) maxRequestBody() int64 {
	if c.gitMaxRequestBody > 0 {
		return c.gitMaxRequestBody
	}
	return defaultMaxGitRequestBody
}

func (c *serverConfig) gitBegin() { c.gitActive.Add(1) }
func (c *serverConfig) gitEnd()   { c.gitActive.Add(-1) }

// waitForGitCommands waits up to d for cancelled git children to exit.
func (c *serverConfig) waitForGitCommands(d time.Duration) {
	deadline := time.Now().Add(d)
	for c.gitActive.Load() > 0 {
		if time.Now().After(deadline) {
			logInfo("git    | gave up waiting %s for %d git process(es) to exit", d, c.gitActive.Load())
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// isGitRequest decides whether a path belongs to the git endpoints.
//
// Under a prefix such as /git/ the prefix alone settles it. Mounted at the root
// ("--http-git-repos-prefix /") every path would match, so the repo segment has to
// look like one: a git URL always starts with a "*.git" segment, which is what
// lets root-level clone URLs coexist with /pulse and the go-import paths.
func isGitRequest(cfg *serverConfig, path string) bool {
	if !strings.HasPrefix(path, cfg.httpGitReposPrefix) {
		return false
	}
	if cfg.httpGitReposPrefix != "/" {
		return true
	}
	seg, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return seg != ".git" && strings.HasSuffix(seg, ".git")
}

func newRootHandler(cfg *serverConfig) http.Handler {
	if cfg.gitMaxConcurrent <= 0 {
		cfg.gitMaxConcurrent = defaultGitMaxConcurrent
	}
	if cfg.gitSem == nil {
		cfg.gitSem = make(chan struct{}, cfg.gitMaxConcurrent)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// When a handler returns without consuming the request body, the server
		// drains it so the connection can be reused - and that read carries no
		// deadline of its own. Routes that never reach the git concurrency limit
		// (the root page, 404s, and the 400/403/503 rejections inside serveGit)
		// would otherwise let a client declare a body, send nothing, and hold the
		// socket and its goroutine indefinitely. This defer runs after serveGit's
		// own cleanup, so it is the last word on the connection deadline.
		if cfg.gitIdleTimeout > 0 {
			defer func() {
				_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(cfg.gitIdleTimeout))
			}()
		}

		path := r.URL.Path
		if len(path) > maxURLPathLen {
			http.Error(w, "request path too long", http.StatusRequestURITooLong)
			return
		}

		if isGitRequest(cfg, path) {
			serveGit(w, r, cfg)
			return
		}

		if path == pulsePath {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%s %s\n", banner, version)
			return
		}

		if cfg.enableGoImport {
			serveGoImport(w, r, cfg)
			return
		}

		http.NotFound(w, r)
	})
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := truncateForLog(r.URL.Path)
		logInfo("%s %q start", r.Method, path)
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		logInfo("%s %q  done - %d %s", r.Method, path, rec.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer, so flushing
// and read/write deadlines still work through this wrapper.
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		f.Flush()
	}
}

// truncateForLog keeps a hostile request path from dominating the log.
func truncateForLog(s string) string {
	if len(s) <= maxLoggedPathLen {
		return s
	}
	return s[:maxLoggedPathLen] + "...(truncated)"
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
