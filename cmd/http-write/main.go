package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"
)

var version = "dev"

const banner = "codemodify/simple-git-server-http-write"

func init() {
	log.SetFlags(0)
}

func main() {
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	// No authentication — private-network convenience only. Parent must gate with
	// --enable-http-write; do not expose that flag on the public internet.
	if err := runGitHTTPBackend(); err != nil {
		logInfo("git http-backend: %v", err)
		os.Exit(1)
	}
}

// runGitHTTPBackend execs git http-backend with receive-pack enabled and the
// dumb file-serving protocol disabled.
// CGI environment is inherited from the parent (GIT_PROJECT_ROOT, PATH_INFO, …).
func runGitHTTPBackend() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git binary not found on PATH: %w", err)
	}

	// http.getanyfile=false keeps the dumb protocol off here too, matching the
	// parent's read path: it would otherwise serve unreachable objects by SHA.
	cmd := exec.Command("git", "-c", "http.receivepack=true", "-c", "http.getanyfile=false", "http-backend")
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func logInfo(format string, v ...interface{}) {
	log.Default().Printf("I | %s | %s | %s | %s \n", banner, version, time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, v...))
}
