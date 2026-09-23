package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// allowedOptions is a default-deny allowlist. These options only ever disable a
// capability this server never offers in the first place — it runs one git
// command per session with no pty, no forwarding, no environment and no user rc
// — so honoring them is a no-op and accepting the key is safe. "restrict" is
// included deliberately: it is the most hardened form OpenSSH offers and
// disables exactly those same capabilities.
//
// Every other option (command=, from=, principals=, expiry-time=, cert-authority,
// environment=, permitopen=, permitlisten=, tunnel=, verify-required, …) carries
// access semantics this server cannot enforce, so keys bearing them are refused
// rather than silently treated as unrestricted.
var allowedOptions = map[string]bool{
	"no-pty":              true,
	"no-port-forwarding":  true,
	"no-agent-forwarding": true,
	"no-x11-forwarding":   true,
	"no-user-rc":          true,
	"restrict":            true,
}

// keyStore holds the authorized public keys, reloadable on SIGHUP so that
// revoking a key does not require a restart.
type keyStore struct {
	path string
	keys atomic.Pointer[map[string]ssh.PublicKey]
}

// newKeyStore loads the initial key set. Unlike a reload, starting with no
// usable keys is a configuration error rather than a revocation.
func newKeyStore(path string) (*keyStore, int, error) {
	store := &keyStore{path: path}
	n, err := store.reload()
	if err != nil {
		return nil, 0, err
	}
	if n == 0 {
		return nil, 0, fmt.Errorf("no usable public keys in %s", path)
	}
	return store, n, nil
}

// reload re-reads the authorized_keys file.
//
// A read failure leaves the previous set in place: a transiently unreadable file
// must not lock everyone out. But a file that reads successfully and yields no
// usable keys is a deliberate revocation and is applied as such — retaining the
// old keys there would silently keep revoked access alive, which is how an
// operator emptying the file gets the opposite of what they asked for.
func (k *keyStore) reload() (int, error) {
	raw, err := os.ReadFile(k.path)
	if err != nil {
		return 0, err
	}
	keys, err := parseAuthorizedKeys(raw)
	if err != nil {
		return 0, err
	}
	k.keys.Store(&keys)
	return len(keys), nil
}

func (k *keyStore) authorized(key ssh.PublicKey) bool {
	m := k.keys.Load()
	if m == nil {
		return false
	}
	_, ok := (*m)[string(key.Marshal())]
	return ok
}

// parseAuthorizedKeys parses an OpenSSH authorized_keys file.
// Empty lines and comment lines (#) are skipped; invalid lines are skipped with a log note.
func parseAuthorizedKeys(raw []byte) (map[string]ssh.PublicKey, error) {
	out := make(map[string]ssh.PublicKey)
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		pub, comment, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			logInfo("auth   | skip authorized_keys line: %v", err)
			continue
		}
		if blocking := unenforceableOptions(options); len(blocking) > 0 {
			// Refuse rather than ignore: honoring "command=" is the entire point
			// of such a key, and dropping it would widen access.
			logInfo("auth   | REFUSED key %q: authorized_keys option(s) %s cannot be enforced by this server; remove them or use OpenSSH + git-shell",
				comment, strings.Join(blocking, ","))
			continue
		}
		out[string(pub.Marshal())] = pub
	}
	return out, nil
}

// unenforceableOptions returns every option on a key that is not in the
// allowlist, i.e. everything this server cannot honor.
func unenforceableOptions(options []string) []string {
	var found []string
	for _, opt := range options {
		name := strings.ToLower(strings.TrimSpace(opt))
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		if !allowedOptions[name] {
			found = append(found, opt)
		}
	}
	return found
}

func publicKeyCallback(store *keyStore) func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
	return func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if store.authorized(key) {
			return &ssh.Permissions{
				Extensions: map[string]string{
					"pubkey-fp": ssh.FingerprintSHA256(key),
				},
			}, nil
		}
		return nil, fmt.Errorf("public key not authorized for user %q", conn.User())
	}
}
