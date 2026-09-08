package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var version = "dev"

const (
	banner         = "codemodify/simple-git-server"
	defaultHTTPOn  = "127.0.0.1:64180"
	defaultHTTPSOn = "127.0.0.1:64143"
	defaultRepos   = "."
	defaultPrefix  = "/git/"
)

func init() {
	log.SetFlags(0)
}

func main() {
	showVersion := flag.Bool("version", false, "print version")

	gitReposFolder := flag.String("git-repos-folder", defaultRepos, "root folder for git repos")
	gitURLPrefix := flag.String("git-url-prefix", defaultPrefix, "URL path prefix for git server")

	enableHTTPRead := flag.Bool("enable-http-read", true, "enable HTTP listener (read)")
	httpOn := flag.String("http-on", defaultHTTPOn, "HTTP bind address")

	enableHTTPSRead := flag.Bool("enable-https-read", false, "enable HTTPS listener (read)")
	httpsOn := flag.String("https-on", defaultHTTPSOn, "HTTPS bind address")
	httpsCert := flag.String("https-cert", "", "HTTPS Cert")
	httpsKey := flag.String("https-key", "", "HTTPS Key")

	// HTTP push: per-request simple-git-server-http-push helper (CGI), gated separately from read-only http-backend.
	// No auth — private networks only; never expose --enable-http-write on the public internet.
	enableHTTPWrite := flag.Bool("enable-http-write", false, "allow unauthenticated git-receive-pack (push) over HTTP via simple-git-server-http-push (private network only)")
	httpPushBinary := flag.String("http-push-binary", defaultHTTPPushBinary, "simple-git-server-http-push binary name or absolute path")

	// SSH push: supervise long-lived simple-git-server-ssh-push child (not per-request like http-backend).
	enableSSHRead := flag.Bool("enable-ssh-read", false, "enable supervised SSH git access via simple-git-server-ssh-push")
	sshOn := flag.String("ssh-on", defaultSSHOn, "SSH bind address (passed to ssh-push)")
	sshHostKey := flag.String("ssh-host-key", "", "path to SSH host private key (required if --enable-ssh-read)")
	sshAuthorizedKeys := flag.String("ssh-authorized-keys", "", "path to authorized_keys (required if --enable-ssh-read)")
	sshPushBinary := flag.String("ssh-push-binary", defaultSSHPushBinary, "simple-git-server-ssh-push binary name or absolute path")
	enableSSHWrite := flag.Bool("enable-ssh-write", true, "allow upload-pack (fetch/clone) over SSH; passed through to supervised simple-git-server-ssh-push")

	enableGoImport := flag.Bool("enable-go-import", false, "serve go-import meta pages")
	goImportDomain := flag.String("go-import-domain", "", "domain for go-import; required when --enable-go-import")
	goImportGitURL := flag.String("go-import-git-url", "", "override template for repo URL; default https://{domain}{gitURLPrefix}{repo}.git")
	gitMaxConcurrent := flag.Int("git-max-concurrent", 32, "max concurrent git http-backend processes")

	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	sshPushOpts := sshPushOptions{
		enable:         *enableSSHRead,
		sshOn:          *sshOn,
		hostKey:        *sshHostKey,
		authorizedKeys: *sshAuthorizedKeys,
		sshPushBinary:  *sshPushBinary,
		enableSSHWrite: *enableSSHWrite,
	}
	httpPushOpts := httpPushOptions{
		enable:         *enableHTTPWrite,
		httpPushBinary: *httpPushBinary,
	}

	cfg, err := buildConfig(*goImportDomain, *gitReposFolder, *enableHTTPRead, *httpOn, *enableHTTPSRead, *httpsOn, *httpsCert, *httpsKey, *enableGoImport, *gitURLPrefix, *goImportGitURL, sshPushOpts, httpPushOpts)
	if err != nil {
		logError("%v", err)
	}
	if *gitMaxConcurrent > 0 {
		cfg.gitMaxConcurrent = *gitMaxConcurrent
	}

	if _, err := exec.LookPath("git"); err != nil {
		logError("git binary not found on PATH (required for git server): %v", err)
	}
	if cfg.enableHTTPWrite {
		if _, err := resolveHTTPPushBinary(cfg.httpPushBinary); err != nil {
			logError("%v", err)
		}
	}

	handler := accessLog(newRootHandler(cfg))

	if cfg.enableHTTPRead {
		logInfo("listen | http://%s", cfg.httpOn)
	}
	if cfg.enableHTTPSRead {
		logInfo("listen | https://%s", cfg.httpsOn)
	}
	logInfo("gitReposFolder | %s", cfg.gitReposFolder)
	if cfg.enableGoImport {
		logInfo("go-import | domain=%s prefix=%s", cfg.goImportDomain, cfg.gitURLPrefix)
	}
	if cfg.enableHTTPWrite {
		logInfo("git    | git server on %s (HTTP push via %s; NO AUTH — private network only; GIT_HTTP_EXPORT_ALL=1)", cfg.gitURLPrefix, cfg.httpPushBinary)
	} else {
		logInfo("git    | git server on %s (read-only HTTP; receive-pack off; GIT_HTTP_EXPORT_ALL=1)", cfg.gitURLPrefix)
	}

	const (
		readHeaderTimeout = 10 * time.Second
		idleTimeout       = 120 * time.Second
		shutdownTimeout   = 5 * time.Second
	)

	var ssh *sshChild
	if cfg.enableSSHRead {
		binary, err := resolveSSHPushBinary(cfg.sshPushBinary)
		if err != nil {
			logError("%v", err)
		}
		ssh, err = startSSHPush(cfg, binary)
		if err != nil {
			logError("start simple-git-server-ssh-push: %v", err)
		}
		logInfo("ssh    | supervised simple-git-server-ssh-push pid=%d listen=%s binary=%s (long-lived child, not per-push)", ssh.cmd.Process.Pid, cfg.sshOn, binary)
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
			serverErr = fmt.Errorf("simple-git-server-ssh-push exited unexpectedly: %w", err)
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

	if ssh != nil && !sshExited {
		logInfo("ssh    | stopping supervised simple-git-server-ssh-push")
		ssh.stop(sshChildStopTimeout)
	}

	if serverErr != nil {
		os.Exit(1)
	}
}

type serverConfig struct {
	enableHTTPRead bool
	httpOn         string

	enableHTTPSRead bool
	httpsOn             string
	httpsCert           string
	httpsKey            string

	gitReposFolder string
	gitURLPrefix   string

	enableGoImport bool
	goImportDomain string
	goImportGitURL string // template with {domain} (=goImportDomain) and {repo} placeholders

	gitMaxConcurrent int           // max concurrent git http-backend processes
	gitSem           chan struct{} // semaphore; sized in newRootHandler

	// SSH push supervision (long-lived simple-git-server-ssh-push child; not per-request).
	enableSSHRead     bool
	sshOn             string
	sshHostKey        string
	sshAuthorizedKeys string
	sshPushBinary     string
	enableSSHWrite    bool

	// HTTP push via per-request simple-git-server-http-push helper (CGI). No auth — private network only.
	enableHTTPWrite bool
	httpPushBinary      string
}

func buildConfig(goImportDomain, gitReposFolder string, enableHTTPRead bool, httpOn string, enableHTTPSRead bool, httpsOn, httpsCert, httpsKey string, enableGoImport bool, gitURLPrefix, goImportGitURL string, ssh sshPushOptions, httpPush httpPushOptions) (*serverConfig, error) {
	if !enableHTTPRead && !enableHTTPSRead {
		return nil, errors.New("either '--enable-http-read' or '--enable-https-read' is required")
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

	prefix := gitURLPrefix
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
	binary := ssh.sshPushBinary
	if binary == "" {
		binary = defaultSSHPushBinary
	}
	httpBinary := httpPush.httpPushBinary
	if httpBinary == "" {
		httpBinary = defaultHTTPPushBinary
	}

	return &serverConfig{
		enableHTTPRead: enableHTTPRead,
		httpOn:         httpOn,

		enableHTTPSRead: enableHTTPSRead,
		httpsOn:             httpsOn,
		httpsCert:           httpsCert,
		httpsKey:            httpsKey,

		gitReposFolder: gitReposFolder,
		gitURLPrefix:   prefix,

		enableGoImport: enableGoImport,
		goImportDomain: strings.TrimSpace(goImportDomain),
		goImportGitURL: template,

		gitMaxConcurrent: 32,

		enableSSHRead:     ssh.enable,
		sshOn:             sshOn,
		sshHostKey:        ssh.hostKey,
		sshAuthorizedKeys: ssh.authorizedKeys,
		sshPushBinary:     binary,
		enableSSHWrite:    ssh.enableSSHWrite,

		enableHTTPWrite: httpPush.enable,
		httpPushBinary:      httpBinary,
	}, nil
}

func newRootHandler(cfg *serverConfig) http.Handler {
	if cfg.gitMaxConcurrent <= 0 {
		cfg.gitMaxConcurrent = 32
	}
	if cfg.gitSem == nil {
		cfg.gitSem = make(chan struct{}, cfg.gitMaxConcurrent)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if strings.HasPrefix(path, cfg.gitURLPrefix) {
			serveGit(w, r, cfg)
			return
		}

		if path == "/" {
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
		logInfo("%s %s start", r.Method, r.URL.Path)
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		logInfo("%s %s  done - %d %s", r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
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

func logInfo(format string, v ...interface{}) {
	log.Default().Printf("I | %s | %s | %s | %s \n", banner, version, timeNowAsString(), fmt.Sprintf(format, v...))
}

func logError(format string, v ...interface{}) {
	log.Default().Fatalf("E | %s | %s | %s | %s \n", banner, version, timeNowAsString(), fmt.Sprintf(format, v...))
}

func timeNowAsString() string {
	return time.Now().UTC().Format(time.RFC3339)
}
