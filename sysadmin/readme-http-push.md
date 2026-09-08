# simple-git-server-http-push — unauthenticated HTTP(S) push helper (private network only)

Optional **write** path for `git-receive-pack` over HTTP(S). Public clone/fetch stays on the parent’s read-only `git http-backend` (`http.receivepack=false`). Push is gated behind `--enable-http-write` and a **per-request** helper binary that runs `git -c http.receivepack=true http-backend`.

> **WARNING — no authentication.** `--enable-http-write` is for **private networks for convenience** only. Anyone who can reach the HTTP(S) listener can push. **Never expose `--enable-http-write` on the public internet.** Prefer SSH + authorized keys for internet-facing write access.

Binary: build `cmd/http-push` to `simple-git-server-http-push` (separate from the HTTP server and from `simple-git-server-ssh-push`).

## How it differs from SSH push

| | HTTP push helper | SSH push (`simple-git-server-ssh-push`) |
| --- | --- | --- |
| Lifetime | **per receive-pack request** (CGI-style) | long-lived listener (standalone or supervised) |
| Auth | **none** (private network only) | authorized_keys |
| Parent flag | `--enable-http-write` | `--enable-ssh-read` |

## Build

`.gobuild-binary` is a **single** name — gobuild keeps **`simple-git-server`** as the primary release binary. Build the HTTP push helper yourself:

```sh
go build -o simple-git-server-http-push ./cmd/http-push
# or:
go build -o /usr/local/bin/simple-git-server-http-push ./cmd/http-push
```

## Enable on the parent

```sh
go build -o /usr/local/bin/simple-git-server-http-push ./cmd/http-push

# Bind to a private / loopback address only — never the public internet with this flag.
simple-git-server \
  --git-repos-folder /home/git \
  --http-on 127.0.0.1:64180 \
  --enable-http-write \
  --http-push-binary simple-git-server-http-push
```

Parent behavior:

1. Non–receive-pack → existing read-only `git -c http.receivepack=false http-backend`
2. receive-pack (`?service=git-receive-pack` or path `…/git-receive-pack`):
   - if `--enable-http-write` is **off** → **403 Forbidden**
   - if **on** (and helper binary present) → exec `simple-git-server-http-push` with CGI env (no credentials)
3. Helper runs `git -c http.receivepack=true http-backend` (no Basic auth check)

Startup requires the helper binary on `PATH` (or an absolute `--http-push-binary`). No user/password flags.

## Parent flags (HTTP push)

| flag | description | default |
| --- | --- | --- |
| `--enable-http-write` | allow **unauthenticated** receive-pack over HTTP via helper (**private network only**) | `false` |
| `--http-push-binary` | helper name or absolute path | `simple-git-server-http-push` |

## Client push example

```sh
# no credentials — anyone who can reach the listener can push
git push http://127.0.0.1:64180/git/cliflags.git main
```

## Notes

- Public clone unchanged — no auth required for upload-pack / info/refs with `service=git-upload-pack`.
- Do **not** set `http.receivepack=true` on bare repos for the default backend path; only the helper enables receive-pack.
- SSH write paths remain unchanged (preferred for public / internet-facing write).
- **Never** put `--enable-http-write` behind a public listener without a separate network ACL / VPN.

## Related

- Access model: [readme-access.md](./readme-access.md)
- SSH push: [readme-ssh-push.md](./readme-ssh-push.md)
- HTTP server unit: [simple-git-server.service](./simple-git-server.service)
