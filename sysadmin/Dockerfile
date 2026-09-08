FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /simple-git-server .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates git && \
    mkdir -p /var/git
COPY --from=build /simple-git-server /usr/local/bin/simple-git-server

USER nobody

VOLUME ["/var/git"]

EXPOSE 64180
ENTRYPOINT ["simple-git-server", "--domain", "goframework.io", "--repos", "/var/git", "--enableHTTP", "--httpOn", ":64180", "--enableVanity", "--enableGitHTTP", "--gitPathPrefix", "/git/"]
