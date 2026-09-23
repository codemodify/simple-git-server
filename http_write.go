package main

import (
	"fmt"
	"os/exec"
)

// simple-git-server-http-write is invoked PER receive-pack request (CGI-style),
// unlike simple-git-server-ssh-read-write which is a long-lived supervised listener.
// When --enable-http-write is set, serveGit execs this helper instead of the
// read-only git http-backend for git-receive-pack only.
//
// No authentication: intended for private networks only. Never expose
// --enable-http-write on the public internet.

const defaultHTTPWriteBinary = "simple-git-server-http-write"

type httpWriteOptions struct {
	enable          bool
	httpWriteBinary string
}

func defaultHTTPWriteOptions() httpWriteOptions {
	return httpWriteOptions{
		enable:          false,
		httpWriteBinary: defaultHTTPWriteBinary,
	}
}

func resolveHTTPWriteBinary(name string) (string, error) {
	if name == "" {
		name = defaultHTTPWriteBinary
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("simple-git-server-http-write binary %q not found: %w", name, err)
	}
	return path, nil
}
