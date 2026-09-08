# what
- tiny git server over HTTP(S) with optional go-import and optional push helpers
- **HTTP read** (clone/fetch): always via `git http-backend` with `http.receivepack=false`
- **HTTP write** (push): optional `--enable-http-write` execs `simple-git-server-http-push` per receive-pack — **no auth, private networks only**
- **SSH**: optional supervised `simple-git-server-ssh-push` (`--enable-ssh-read`) or OpenSSH; `--enable-ssh-write` allows fetch over SSH (upload-pack)
- sibling of simple-http-fileserver / simple-http-router
- access guide: [sysadmin/readme-access.md](./sysadmin/readme-access.md)

# build
```sh
go build -o simple-git-server .
go build -o simple-git-server-http-push ./cmd/http-push
go build -o simple-git-server-ssh-push ./cmd/ssh-push
```

# config (main binary)
| flag | description | default |
|---|---|---|
| `-version` | print version | |
| `--git-repos-folder` | bare repo root | `.` |
| `--git-url-prefix` | URL prefix for git HTTP | `/git/` |
| `--enable-http-read` | HTTP listener | `true` |
| `--http-on` | HTTP bind | `127.0.0.1:64180` |
| `--enable-https-read` | HTTPS listener | `false` |
| `--https-on` | HTTPS bind | `127.0.0.1:64143` |
| `--https-cert` / `--https-key` | TLS files | |
| `--enable-http-write` | HTTP push via helper (**no auth**) | `false` |
| `--http-push-binary` | helper name/path | `simple-git-server-http-push` |
| `--enable-ssh-read` | supervise SSH helper | `false` |
| `--ssh-on` | SSH bind (child) | `127.0.0.1:64222` |
| `--ssh-host-key` / `--ssh-authorized-keys` | required if SSH | |
| `--ssh-push-binary` | SSH helper | `simple-git-server-ssh-push` |
| `--enable-ssh-write` | allow upload-pack on SSH | `true` |
| `--enable-go-import` | vanity go-import pages | `false` |
| `--go-import-domain` | required if go-import | |
| `--go-import-git-url` | template `{domain}` `{repo}` | `https://{domain}{prefix}{repo}.git` |
| `--git-max-concurrent` | max concurrent http-backend | `32` |

# local-dev
```sh
mkdir -p repos && git init --bare repos/cliflags.git
go run . --git-repos-folder ./repos --http-on :64180
git clone http://127.0.0.1:64180/git/cliflags.git /tmp/cliflags
```

# design
- streaming CGI, path sanitize (`*.git`), minimal child env, concurrency limit
- never `git checkout` away uncommitted work; prefer commits on `dev`
