FROM golang:1.26-alpine AS build
ARG REPO=https://github.com/codemodify/simple-git-server.git
ARG REF=dev
RUN apk add --no-cache git
RUN git clone --depth 1 --branch "$REF" "$REPO" /src
WORKDIR /src
RUN go build -trimpath -ldflags="-s -w" -o /out/simple-git-server . && \
    go build -trimpath -ldflags="-s -w" -o /out/simple-git-server-http-write ./cmd/http-write && \
    go build -trimpath -ldflags="-s -w" -o /out/simple-git-server-ssh-read-write ./cmd/ssh-read-write

FROM alpine:3.21
# git is not optional: the server shells out to git http-backend / upload-pack /
# receive-pack for every request.
# git-daemon is not optional either: on alpine that package, not `git`, is what
# ships git-http-backend. Without it every HTTP request dies as a 502
# ("git: 'http-backend' is not a git command"). SSH is unaffected.
RUN apk add --no-cache ca-certificates git git-daemon && mkdir -p /var/git
COPY --from=build /out/ /usr/local/bin/

# Runs as nobody, uid/gid 65534. Mounted repos and keys must be owned by 65534 on
# the host: the kernel matches ids, not names. See the readme's docker section.
USER nobody

ENTRYPOINT ["simple-git-server"]
