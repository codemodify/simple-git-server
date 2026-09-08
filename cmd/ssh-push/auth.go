package main

import (
	"bytes"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// loadAuthorizedKeys parses an OpenSSH authorized_keys file.
// Empty lines and comment lines (#) are skipped. Invalid lines are skipped with a log note.
func loadAuthorizedKeys(path string) (map[string]ssh.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseAuthorizedKeys(raw)
}

func parseAuthorizedKeys(raw []byte) (map[string]ssh.PublicKey, error) {
	out := make(map[string]ssh.PublicKey)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			logInfo("auth   | skip authorized_keys line: %v", err)
			continue
		}
		out[string(pub.Marshal())] = pub
	}
	return out, nil
}

func publicKeyCallback(authorized map[string]ssh.PublicKey) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if _, ok := authorized[string(key.Marshal())]; ok {
			return &ssh.Permissions{
				Extensions: map[string]string{
					"pubkey-fp": ssh.FingerprintSHA256(key),
				},
			}, nil
		}
		return nil, fmt.Errorf("public key not authorized for user %q", conn.User())
	}
}
