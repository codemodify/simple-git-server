# access model — HTTPS read-only by default; optional HTTP/SSH write

Policy:

| Action | HTTPS (`simple-git-server`) | SSH (OpenSSH or `simple-git-server-ssh-push`) |
| --- | --- | --- |
| clone / fetch | yes (public) | yes |
| push | **no** by default; **optional** with `--enable-http-write` (**no auth — private network only**) | yes (authorized keys) |

`simple-git-server` serves git over HTTP(S) via `git http-backend` (**read-only** by default). Write access options:

- **optional HTTP push** — `--enable-http-write` + per-request `simple-git-server-http-push` helper; see section **B2** and [readme-http-push.md](./readme-http-push.md)
- **OpenSSH + `git-shell`** (section C below), or
- **standalone `simple-git-server-ssh-push`** — companion Go binary (`./cmd/ssh-push`); see section **C2** and [readme-ssh-push.md](./readme-ssh-push.md), or
- **supervised `simple-git-server-ssh-push`** — `--enable-ssh-read` on `simple-git-server` starts one **long-lived** child (not per-push); see section **C3**

---

## A. Shared bare repos

Create a dedicated system user whose home holds bare repositories:

```sh
sudo useradd --create-home --home-dir /home/git --shell /usr/bin/git-shell git
# or, if you prefer a normal shell for admin and restrict via authorized_keys command= :
# sudo useradd --create-home --home-dir /home/git git

sudo chown -R git:git /home/git
```

Point simple-git-server at that tree:

```sh
--git-repos-folder /home/git
```

Create a bare repo (example: `cliflags`):

```sh
sudo -u git git init --bare /home/git/cliflags.git
sudo chown -R git:git /home/git/cliflags.git
```

With `GIT_HTTP_EXPORT_ALL=1` (this server’s default), every bare repo under `/home/git` is cloneable over HTTPS. If you prefer opt-in export instead, drop `GIT_HTTP_EXPORT_ALL` in code and `touch /home/git/cliflags.git/git-daemon-export-ok`.

---

## B. Public HTTPS read-only

1. Install and run **simple-git-server** (systemd unit already exists — see [simple-git-server.service](./simple-git-server.service)).
2. Optional vanity / `go get`:

```sh
--enable-go-import --go-import-domain goframework.io
```

3. TLS:
	- **behind [simple-http-router](https://github.com/codemodify/simple-http-router)** (recommended for ACME): router terminates HTTPS and proxies plain HTTP to `127.0.0.1:64180` — see [config.sample.goframework.json](./config.sample.goframework.json)
	- **or in-process HTTPS**: `--enable-https-read --https-cert … --https-key …`

4. Verify clone:

```sh
git clone https://goframework.io/git/cliflags.git
```

5. Verify push over HTTPS **fails by default** (expected — receive-pack is disabled unless `--enable-http-write`):

```sh
cd cliflags
echo test >> README.md && git add README.md && git commit -m test
git push   # must fail over https://… when HTTP push is off
```

6. go-import advertises the **HTTPS** clone URL only, e.g.:

```text
goframework.io/cliflags git https://goframework.io/git/cliflags.git
```

```sh
curl -s 'https://goframework.io/cliflags?go-get=1'
```

By default HTTP(S) does not enable `receive-pack` on the read-only backend. Do not set `http.receivepack=true` on the bare repos for that path — only the optional HTTP push helper enables receive-pack (**no authentication**; private network only).

---

## B2. Optional: HTTP push (no auth — private network only)

Public clone stays unchanged. To allow push over HTTP(S) on a **private network**:

> **WARNING:** `--enable-http-write` has **no authentication**. Anyone who can reach the listener can push. **Never expose it on the public internet.** Prefer SSH for internet-facing write.

```sh
go build -o /usr/local/bin/simple-git-server-http-push ./cmd/http-push

# Bind privately (loopback / VPN / LAN ACL) — never a public address with this flag.
simple-git-server \
  --git-repos-folder /home/git \
  --http-on 127.0.0.1:64180 \
  --enable-http-write \
  --http-push-binary simple-git-server-http-push
```

Client (no credentials):

```sh
git push http://127.0.0.1:64180/git/cliflags.git main
```

Full guide: [readme-http-push.md](./readme-http-push.md).

---

## C. SSH write (OpenSSH)

Enable pubkey authentication for user `git` (password login for `git` should stay off).

### authorized_keys

```sh
sudo -u git mkdir -p /home/git/.ssh
sudo -u git chmod 700 /home/git/.ssh
sudo -u git touch /home/git/.ssh/authorized_keys
sudo -u git chmod 600 /home/git/.ssh/authorized_keys
```

Append a developer’s **public** key. Prefer restricting the key so it can only run git:

```text
command="git-shell -c \"$SSH_ORIGINAL_COMMAND\"",no-port-forwarding,no-X11-forwarding,no-agent-forwarding,no-pty ssh-ed25519 AAAA… developer@example
```

Alternatively, set `git`’s login shell to `git-shell` (`useradd --shell /usr/bin/git-shell` or `chsh`) and store plain pubkeys in `authorized_keys`.

### client `~/.ssh/config`

```text
Host goframework.io
	HostName goframework.io
	User git
	IdentityFile ~/.ssh/id_ed25519_goframework
```

### clone / push over SSH

Path form (common with `~git` as home):

```sh
git clone git@goframework.io:cliflags.git
```

Equivalent `ssh://` form:

```sh
git clone ssh://git@goframework.io/home/git/cliflags.git
# or, depending on sshd/AuthorizedKeysCommand layout:
# git clone ssh://git@goframework.io/~/cliflags.git
```

Push over SSH works for authorized keys. HTTPS remains read-only for everyone (including developers).

---

## C2. Option: simple-git-server-ssh-push instead of OpenSSH

If you do not want to expose system OpenSSH (or prefer an in-process, git-only SSH listener), use the companion binary:

```sh
go build -o /usr/local/bin/simple-git-server-ssh-push ./cmd/ssh-push
```

Point it at the **same** `--git-repos-folder` as `simple-git-server`. Public-key auth from an `authorized_keys` file; only `git-receive-pack` / `git-upload-pack` are accepted (no shell).

```sh
simple-git-server-ssh-push \
  --git-repos-folder /home/git \
  --ssh-on 127.0.0.1:64222 \
  --ssh-host-key /etc/ssh-push/host_key \
  --ssh-authorized-keys /etc/ssh-push/authorized_keys
```

Client (non-default port):

```text
Host goframework-git-write
	HostName goframework.io
	User git
	Port 64222
	IdentityFile ~/.ssh/id_ed25519_goframework
```

```sh
git push ssh://git@goframework-git-write/cliflags.git main
```

Full guide + systemd sample: [readme-ssh-push.md](./readme-ssh-push.md), [ssh-push.service](./ssh-push.service).

HTTP(S) remains read-only on the default backend either way — do not set `http.receivepack=true` on bare repos; optional HTTP push uses the separate helper (section B2).

---

## C3. Option: supervise simple-git-server-ssh-push from simple-git-server

Instead of a separate systemd unit for `simple-git-server-ssh-push`, pass `--enable-ssh-read` so **simple-git-server** starts `simple-git-server-ssh-push` as a **child process** and stops it on SIGINT/SIGTERM.

This is **not** like `git http-backend` (per-request CGI). The SSH server must stay listening for the life of the parent.

```sh
# build the companion once and put it on PATH (or pass absolute --ssh-push-binary)
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

If the child exits unexpectedly, the parent logs and exits so a process supervisor (e.g. systemd) can restart both. Do **not** also run `ssh-push.service` against the same `--ssh-on` port.

## D. Quick matrix

| Action | HTTPS | SSH |
| --- | --- | --- |
| clone / fetch | yes (public) | yes |
| push | no by default; yes with `--enable-http-write` (**no auth — private network only**) | yes (authorized keys) |

---

## Related

- Main readme: [../readme.md](../readme.md)
- Systemd unit (HTTP): [simple-git-server.service](./simple-git-server.service)
- Systemd unit (SSH write): [ssh-push.service](./ssh-push.service)
- simple-git-server-ssh-push guide: [readme-ssh-push.md](./readme-ssh-push.md)
- simple-git-server-http-push guide: [readme-http-push.md](./readme-http-push.md)
- Router sample: [config.sample.goframework.json](./config.sample.goframework.json)
