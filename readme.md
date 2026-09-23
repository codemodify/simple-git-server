### what
- Git server that runs standalone OR behind [simple-http-router](https://github.com/codemodify/simple-http-router) for
	- robust HTTP routing
	- automatic HTTPS
	- single-server-multi-domain setups
- Supports mixing read/write transports
	- can do `--enable-http-read` + `--enable-ssh-read` + `--enable-ssh-write`
	- can do `--enable-ssh-read` + `--enable-ssh-write` and HTTP off
	- can do `--enable-http-read` + `--enable-http-write` and SSH off
```
--enable-http-read / --enable-http-write -> git clone https://domain.com/repo1.git
--enable-ssh-read  / --enable-ssh-write  -> git clone ssh://domain.com/repo1.git
--enable-go-import                       -> go get domain.com/repo1@v1
```

### setup + run (binary)
```sh
# example: git server + read/write over HTTP
export GIT_REPOS_FOLDER=/home/git/repos
mkdir -p $GIT_REPOS_FOLDER

git init --bare $GIT_REPOS_FOLDER/repo1.git
git init --bare $GIT_REPOS_FOLDER/repo2.git

simple-git-server \
  --git-repos-folder $GIT_REPOS_FOLDER \
  --http-on 127.0.0.1:64180 \
  --enable-http-read \
  --enable-http-write

git clone http://127.0.0.1:64180/repo1.git
```

```sh
# example: git server + read over HTTP + read/write over SSH - option A
# - uses existing OS provided SSHD, everything SSH-related is already in place
#	- sshd running, pubkey auth enabled, port 22 reachable, host keys generated
# - simple-git-server serves HTTP only, it takes no part in the SSH path
#	- sshd logs clients into the git account, git-shell runs the git commands
export GIT_REPOS_FOLDER=/home/git/repos
mkdir -p $GIT_REPOS_FOLDER

# the git account is what sshd authenticates into, git-shell keeps it to git only
sudo useradd --create-home --home-dir $GIT_REPOS_FOLDER --shell /usr/bin/git-shell git

sudo -u git git init --bare $GIT_REPOS_FOLDER/repo1.git
sudo -u git git init --bare $GIT_REPOS_FOLDER/repo2.git

# sshd reads the keys from the git account's home, which is $GIT_REPOS_FOLDER above
sudo -u git mkdir -p  $GIT_REPOS_FOLDER/.ssh
sudo -u git touch     $GIT_REPOS_FOLDER/.ssh/authorized_keys
sudo -u git chmod 700 $GIT_REPOS_FOLDER/.ssh
sudo -u git chmod 600 $GIT_REPOS_FOLDER/.ssh/authorized_keys
cat ~/.ssh/id_ed25519.pub | sudo -u git tee -a $GIT_REPOS_FOLDER/.ssh/authorized_keys

simple-git-server \
  --git-repos-folder $GIT_REPOS_FOLDER \
  --enable-http-read \
  --http-on 127.0.0.1:64180

git clone http://127.0.0.1:64180/repo1.git
git clone git@127.0.0.1:repo2.git
```

```sh
# example: git server + read over HTTP + read/write over SSH - option B
# - uses built-in ssh helper, no OS provided SSHD needed
# - needs simple-git-server-ssh-read-write on PATH, no binary no SSH
# - we are the ssh server here, so we need our own host key + authorized_keys
export GIT_REPOS_FOLDER=/home/git/repos
mkdir -p $GIT_REPOS_FOLDER

export GIT_KEYS_FOLDER=/home/git/.ssh/
mkdir -p $GIT_KEYS_FOLDER

# the account only owns the files; nothing logs in as it, so no shell
sudo useradd --create-home --home-dir $GIT_REPOS_FOLDER --shell /usr/sbin/nologin git

sudo -u git git init --bare $GIT_REPOS_FOLDER/repo1.git
sudo -u git git init --bare $GIT_REPOS_FOLDER/repo2.git

sudo ssh-keygen -t ed25519 -f $GIT_KEYS_FOLDER/host_key -N ""
sudo touch $GIT_KEYS_FOLDER/authorized_keys
sudo chmod 600 $GIT_KEYS_FOLDER/host_key $GIT_KEYS_FOLDER/authorized_keys
sudo chown -R git:git $GIT_KEYS_FOLDER

simple-git-server \
  --git-repos-folder $GIT_REPOS_FOLDER \
  --enable-http-read \
  --http-on 127.0.0.1:64180 \
  --enable-ssh-read \
  --enable-ssh-write \
  --ssh-on 127.0.0.1:64222 \
  --ssh-host-key $GIT_KEYS_FOLDER/host_key \
  --ssh-authorized-keys $GIT_KEYS_FOLDER/authorized_keys

git clone http://127.0.0.1:64180/repo1.git
git clone ssh://127.0.0.1:64222/repo2.git
```

### setup + run (docker)
```sh
docker build -t simple-git-server https://github.com/codemodify/simple-git-server.git#dev:sysadmin
```

```sh
# example: git server + read/write over HTTP
export GIT_REPOS_FOLDER=/home/git/repos
mkdir -p $GIT_REPOS_FOLDER

git init --bare $GIT_REPOS_FOLDER/repo1.git
git init --bare $GIT_REPOS_FOLDER/repo2.git

# Dockerfile runs under `nobody` uid/gid `65534`
# Dockerfile `nobody` does not map to OS `nobody` but `65534` does (go figure)
sudo chown -R 65534:65534 $GIT_REPOS_FOLDER

docker run \
  -p 64180:64180 \
  -v $GIT_REPOS_FOLDER:/var/git \
  simple-git-server \
    --git-repos-folder /var/git \
    --http-on :64180 \
    --enable-http-read \
    --enable-http-write

git clone http://127.0.0.1:64180/repo1.git
```

```sh
# example: git server + read over HTTP + read/write over SSH - option B
# - uses built-in ssh helper, no OS provided SSHD needed
# - the helper ships inside the image, nothing extra to install
# - we are the ssh server here, so we need our own host key + authorized_keys
export GIT_REPOS_FOLDER=/home/git/repos
mkdir -p $GIT_REPOS_FOLDER

export GIT_KEYS_FOLDER=/home/git/.ssh/
mkdir -p $GIT_KEYS_FOLDER

git init --bare $GIT_REPOS_FOLDER/repo1.git
git init --bare $GIT_REPOS_FOLDER/repo2.git

ssh-keygen -t ed25519 -f $GIT_KEYS_FOLDER/host_key -N ""
touch $GIT_KEYS_FOLDER/authorized_keys
chmod 600 $GIT_KEYS_FOLDER/host_key $GIT_KEYS_FOLDER/authorized_keys

# Dockerfile runs under `nobody` uid/gid `65534`
sudo chown -R 65534:65534 $GIT_REPOS_FOLDER $GIT_KEYS_FOLDER

docker run \
  -p 64180:64180 \
  -p 64222:64222 \
  -v $GIT_REPOS_FOLDER:/var/git \
  -v $GIT_KEYS_FOLDER:/keys:ro \
  simple-git-server \
    --git-repos-folder /var/git \
    --enable-http-read \
    --http-on :64180 \
    --enable-ssh-read \
    --enable-ssh-write \
    --ssh-on :64222 \
    --ssh-host-key /keys/host_key \
    --ssh-authorized-keys /keys/authorized_keys

git clone http://127.0.0.1:64180/repo1.git
git clone ssh://127.0.0.1:64222/repo2.git
```


### get the binary
- `curl -fsSL https://raw.githubusercontent.com/codemodify/simple-git-server/dev/install.sh | sh`
- OR build
	```sh
	git clone https://github.com/codemodify/simple-git-server.git
	cd simple-git-server

	go build -trimpath -ldflags="-s -w" -o simple-git-server .
	go build -trimpath -ldflags="-s -w" -o simple-git-server-http-write ./cmd/http-write
	go build -trimpath -ldflags="-s -w" -o simple-git-server-ssh-read-write ./cmd/ssh-read-write

	sudo cp simple-git-server                /usr/local/bin/
	sudo cp simple-git-server-http-write     /usr/local/bin/
	sudo cp simple-git-server-ssh-read-write /usr/local/bin/
	```

### flags
|									| default									| description
|---								|---										|---
| `--version`						| 											| print version
| `--git-repos-folder`				| `/home/git`								| repos folder
| **http**							| 											|
| `--enable-http-read`				| `true`									| HTTP listener on/off
| `--http-on`						| `127.0.0.1:64180`							| HTTP bind
| `--http-git-repos-prefix`			| `/`										| HTTP(s) routing collision safety on a shared domain
| **https** 						| 											|
| `--enable-https-read`				| `false`									| HTTPS listener on/off
| `--https-on`						| `127.0.0.1:64143`							| HTTPS bind
| `--https-cert`					| 											| TLS files
| `--https-key`						| 											| TLS files
| **http write**					| 											|
| `--enable-http-write`				| `false`									| push over HTTP, convenience for local networks only, no auth
| `--http-write-binary`				| `simple-git-server-http-write`			| binary that enables http-write, no binary no capability
| **ssh**							| 											|
| `--enable-ssh-read`				| `false`									| start supervised SSH helper; allows clone/fetch
| `--ssh-read-write-binary`			| `simple-git-server-ssh-read-write`		| binary that enables SSH read/write, no binary no capability
| `--ssh-on`						| `127.0.0.1:64222`							| SSH bind
| `--ssh-host-key`					| 											| required with SSH
| `--ssh-authorized-keys`			| 											| required with SSH
| **ssh write**						| 											|
| `--enable-ssh-write`				| `false`									| allow push over SSH; needs `--enable-ssh-read`
| **go-import**						| 											|
| `--enable-go-import`				| `false`									| Go lang import support
| `--go-import-domain`				|											| required with go-import
| `--go-import-git-url`				| `https://{domain}{prefix}{repo}.git`		| template, `{domain}` + `{repo}`
| **limits**						|											|
| `--git-max-concurrent`			| `32`										| max concurrent git processes
| `--git-idle-timeout`				| `60s`										| kill a request that stops making progress; `0` disables
| `--git-max-request-body`			| `104857600`								| max request body in bytes — also caps the pack on a push

