package main

import (
	"fmt"
	"os/exec"
)

// simple-git-server-http-push is invoked PER receive-pack request (CGI-style),
// unlike simple-git-server-ssh-push which is a long-lived supervised listener.
// When --enable-http-write is set, serveGit execs this helper instead of the
// read-only git http-backend for git-receive-pack only.
//
// No authentication: intended for private networks only. Never expose
// --enable-http-write on the public internet.

const defaultHTTPPushBinary = "simple-git-server-http-push"

type httpPushOptions struct {
	enable         bool
	httpPushBinary string
}

func defaultHTTPPushOptions() httpPushOptions {
	return httpPushOptions{
		enable:         false,
		httpPushBinary: defaultHTTPPushBinary,
	}
}

func resolveHTTPPushBinary(name string) (string, error) {
	if name == "" {
		name = defaultHTTPPushBinary
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("simple-git-server-http-push binary %q not found: %w", name, err)
	}
	return path, nil
}
