package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// simple-git-server-ssh-read-write is a long-lived SSH listener (receive-pack / upload-pack).
// Unlike git http-backend, it is NOT spawned per request: when --enable-ssh-read
// is set, simple-git-server starts one child and keeps it running until shutdown.

const (
	defaultSSHOn              = "127.0.0.1:64222"
	defaultSSHReadWriteBinary = "simple-git-server-ssh-read-write"
	sshChildStopTimeout       = 5 * time.Second
)

type sshReadWriteOptions struct {
	enable             bool
	sshOn              string
	hostKey            string
	authorizedKeys     string
	sshReadWriteBinary string
	enableSSHWrite     bool
}

func defaultSSHReadWriteOptions() sshReadWriteOptions {
	return sshReadWriteOptions{
		enable:             false,
		sshOn:              defaultSSHOn,
		sshReadWriteBinary: defaultSSHReadWriteBinary,
		enableSSHWrite:     false,
	}
}

// sshReadWriteArgs builds argv for the supervised simple-git-server-ssh-read-write child.
func sshReadWriteArgs(cfg *serverConfig) []string {
	return []string{
		"--git-repos-folder", cfg.gitReposFolder,
		"--ssh-on", cfg.sshOn,
		"--ssh-host-key", cfg.sshHostKey,
		"--ssh-authorized-keys", cfg.sshAuthorizedKeys,
		"--enable-ssh-read=" + strconv.FormatBool(cfg.enableSSHRead),
		"--enable-ssh-write=" + strconv.FormatBool(cfg.enableSSHWrite),
		"--git-max-concurrent=" + strconv.Itoa(cfg.gitMaxConcurrent),
		"--git-idle-timeout=" + cfg.gitIdleTimeout.String(),
	}
}

func resolveSSHReadWriteBinary(name string) (string, error) {
	if name == "" {
		name = defaultSSHReadWriteBinary
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("simple-git-server-ssh-read-write binary %q not found: %w", name, err)
	}
	return path, nil
}

// sshChild is a supervised long-lived simple-git-server-ssh-read-write process.
type sshChild struct {
	cmd  *exec.Cmd
	done chan error
}

func startSSHReadWrite(cfg *serverConfig, binary string) (*sshChild, error) {
	cmd := exec.Command(binary, sshReadWriteArgs(cfg)...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Own process group so parent SIGINT/SIGTERM is not double-delivered oddly,
	// and we can signal the whole group on shutdown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	child := &sshChild{
		cmd:  cmd,
		done: make(chan error, 1),
	}
	go func() {
		child.done <- cmd.Wait()
	}()
	return child, nil
}

// stop sends SIGTERM to the child's process group, waits, then SIGKILL.
func (c *sshChild) stop(timeout time.Duration) {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	select {
	case <-c.done:
		return
	default:
	}
	pgid := c.cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	select {
	case <-c.done:
		return
	case <-time.After(timeout):
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		<-c.done
	}
}
