# simple-git-server-ssh-push — in-Go SSH write path

Alternative to OpenSSH + `git-shell` for **push** (and optional fetch) against the same bare repos served read-only by `simple-git-server` over HTTP(S).

Binary: build `cmd/ssh-push` to `simple-git-server-ssh-push` (separate from the HTTP server). Same module; same `--git-repos-folder`.

## Two run modes

1. **Standalone** — run `simple-git-server-ssh-push` yourself (systemd unit, or manually). HTTP and SSH are separate processes.
2. **Supervised** — `simple-git-server --enable-ssh-read` starts **one long-lived** `simple-git-server-ssh-push` child and stops it on shutdown. This is **not** per-push spawning (unlike `git http-backend`); the SSH listener must stay up.

Pick one mode for a given `--ssh-on` bind address — do not run both.

## Build

`.gobuild-binary` is a **single** name — gobuild keeps **`simple-git-server`** as the primary release binary. Build the SSH writer yourself:

```sh
go build -o simple-git-server-ssh-push ./cmd/ssh-push
# or:
go build -o /usr/local/bin/simple-git-server-ssh-push ./cmd/ssh-push
```

## Generate host key

```sh
sudo mkdir -p /etc/ssh-push
sudo ssh-keygen -t ed25519 -f /etc/ssh-push/host_key -N "" -C "simple-git-server-ssh-push"
sudo chmod 600 /etc/ssh-push/host_key
```

## authorized_keys

Plain OpenSSH authorized_keys (one pubkey per line; `#` comments and blank lines ok):

```sh
sudo mkdir -p /etc/ssh-push
sudo touch /etc/ssh-push/authorized_keys
sudo chmod 600 /etc/ssh-push/authorized_keys
# append developer public keys:
# ssh-ed25519 AAAA… developer@example
```

Only listed public keys authenticate. There is no password auth and no shell.

## Run (same repos as HTTP)

### Mode A — supervised by simple-git-server

```sh
go build -o /usr/local/bin/simple-git-server-ssh-push ./cmd/ssh-push

simple-git-server \
  --git-repos-folder /home/git \
  --http-on 127.0.0.1:64180 \
  --enable-ssh-read \
  --ssh-on 127.0.0.1:64222 \
  --ssh-host-key /etc/ssh-push/host_key \
  --ssh-authorized-keys /etc/ssh-push/authorized_keys \
  --ssh-push-binary simple-git-server-ssh-push \
  --enable-ssh-write=true
```

Parent looks up `--ssh-push-binary` on `PATH` (or use an absolute path), starts the child once, logs its pid, and on SIGINT/SIGTERM sends SIGTERM to the child process group (then Kill after timeout). If the child dies while the parent is running, the parent exits so systemd (or similar) notices.

### Mode B — standalone

```sh
# HTTP read-only (existing binary)
simple-git-server --git-repos-folder /home/git --http-on 127.0.0.1:64180

# SSH write (this binary) against the SAME folder
simple-git-server-ssh-push \
  --git-repos-folder /home/git \
  --ssh-on 127.0.0.1:64222 \
  --ssh-host-key /etc/ssh-push/host_key \
  --ssh-authorized-keys /etc/ssh-push/authorized_keys \
  --enable-ssh-write=true
```

Flags:

| flag | description | default |
| --- | --- | --- |
| `-version` | print version and exit | |
| `--git-repos-folder` | bare git repository root | `.` |
| `--ssh-on` | SSH bind address | `127.0.0.1:64222` |
| `--ssh-host-key` | PEM/OpenSSH private host key (**required**) | |
| `--ssh-authorized-keys` | authorized_keys path (**required**) | |
| `--enable-ssh-write` | allow fetch (`git-upload-pack`) over SSH | `true` |

`git-receive-pack` is always allowed for authenticated keys (this binary’s purpose is write).

## systemd

See [ssh-push.service](./ssh-push.service).

```sh
sudo cp sysadmin/ssh-push.service /etc/systemd/system/ssh-push.service
sudo systemctl daemon-reload && sudo systemctl enable ssh-push && sudo systemctl start ssh-push
```

## Client SSH config

Bind to the non-default port (64222) and prefer a dedicated key:

```text
Host goframework-git-write
	HostName goframework.io
	User git
	Port 64222
	IdentityFile ~/.ssh/id_ed25519_goframework
	IdentitiesOnly yes
```

```sh
git clone ssh://git@goframework-git-write/cliflags.git
# or after adding remote:
git remote add write ssh://git@goframework-git-write/cliflags.git
git push write main
```

Put a TLS terminator / firewall in front if binding beyond localhost. Default bind is `127.0.0.1:64222` — expose via SSH reverse proxy, socat, or change `--ssh-on` deliberately.

## Related

- Access model: [readme-access.md](./readme-access.md)
- HTTP server unit: [simple-git-server.service](./simple-git-server.service)
