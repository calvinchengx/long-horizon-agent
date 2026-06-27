# Reference sandbox image

`sandbox/Dockerfile` builds a polyglot image for LHA's Docker sandbox:

| Tool | Version (build arg) |
| --- | --- |
| Go | 1.26.8 (`GO_VERSION`) |
| uv | 0.12.18 (`UV_VERSION`) |
| Python | 3.12, managed by uv (`PYTHON_VERSION`) |
| Node.js | 22.23.2 (`NODE_VERSION`), with npm and corepack |
| pnpm | 10.34.5 (`PNPM_VERSION`) |
| Other | git, make, curl, ca-certificates, openssh-client, xz-utils |

The default image (`ghcr.io/astral-sh/uv:python3.12-bookworm-slim`) covers Python projects. Use this
image when the agent's checks also need Go or Node.

## Build

From the repository root:

```sh
docker build -t lha-sandbox:latest sandbox/
# pin other versions if you need to
docker build -t lha-sandbox:go1.26 --build-arg GO_VERSION=1.26.8 --build-arg PNPM_VERSION=10.34.5 sandbox/
```

## Use it

```sh
export LHA_SANDBOX=docker
export LHA_SANDBOX_IMAGE=lha-sandbox:latest
# Go + Python + npm package registries, and nothing else:
export LHA_SANDBOX_EGRESS="proxy.golang.org,sum.golang.org,pypi.org,files.pythonhosted.org,registry.npmjs.org"
```

`LHA_SANDBOX_EGRESS` is a comma-separated allow-list:

- `pypi.org` allows exactly that host.
- `.golang.org` (with a leading dot) allows `golang.org` and every subdomain.
- A host alone allows ports 443 and 80. `host:port` allows one other port.
- IP addresses are rejected. The allow-list names hosts only.

Registries that commonly need adding:

| Ecosystem | Hosts |
| --- | --- |
| Go modules | `proxy.golang.org`, `sum.golang.org`. Modules that `GOPROXY` can't serve (for example `GOPRIVATE` or `direct`) also need their VCS host, such as `github.com` or `codeload.github.com`. |
| Python (pip/uv) | `pypi.org`, `files.pythonhosted.org` |
| npm/pnpm/yarn | `registry.npmjs.org`. Yarn classic also needs `registry.yarnpkg.com`. |
| corepack (downloading a pinned package manager) | `registry.npmjs.org`, `repo.yarnpkg.com` |

## How egress works

With `LHA_SANDBOX_EGRESS` empty, which is the default, the container runs with `network_mode=none`
and has no network at all.

With a non-empty allow-list, every sandbox session gets:

1. **A network of its own with no route out.** It is created with
   `docker network create --internal` and named `lha-egress-<random>`. The sandbox container joins
   only this network. It can't resolve or reach anything outside it, whatever code the agent runs.
2. **A proxy container.** It is named `lha-egress-proxy-<random>` and runs `python:3.12-alpine`.
   It sits on both the internal network and the default bridge, and runs
   `python/src/lha/execution/egress_proxy.py`. That file uses only the standard library and has no
   dependencies. The proxy is the only way out of the internal network. It:
   - allows a request only when the host is on the allow-list;
   - resolves the host, then refuses if any of the resolved addresses is loopback, private,
     link-local, CGNAT, multicast or reserved (so no Docker host, cloud metadata or internal
     services);
   - connects to the exact address it checked;
   - answers everything else with `403`, and logs one line per request. Read the log with
     `docker logs lha-egress-proxy-…`.
3. **Proxy environment variables in the sandbox.** `HTTP_PROXY`, `HTTPS_PROXY`, `http_proxy` and
   `https_proxy` are set to `http://lha-egress-proxy-<random>:3128`, and `NO_PROXY` to
   `localhost,127.0.0.1`. pip, uv, go, npm, pnpm, curl and git over HTTPS all follow these
   variables. A tool that ignores them fails, because the container has no other route.

Closing the session removes the sandbox container, the proxy container and the network. If `open`
fails, it removes them too.

HTTPS goes through `CONNECT` and TLS stays end to end, so the proxy only ever sees the host name.
It never sees paths or credentials.

## Running as an arbitrary non-root user

DockerSandbox starts containers with these settings:

- `--user <host uid>:<host gid>`
- `HOME=/workspace`
- a read-only root filesystem
- a 1 GB tmpfs at `/tmp`, mounted `exec` (so `go test` can run the binaries it builds there)
- `--cap-drop ALL`
- `no-new-privileges`

The image is built for that setup:

- **Caches go to `/tmp`.** Every cache and store lives under `/tmp`: `GOPATH`, `GOMODCACHE`,
  `GOCACHE`, `UV_CACHE_DIR`, `PIP_CACHE_DIR`, `npm_config_cache`, pnpm's store and `PNPM_HOME`,
  `COREPACK_HOME` and the `XDG_*` directories. Nothing writes to the read-only root filesystem, and
  nothing lands in the workspace, where it would be committed along with the agent's work.
- **Tools are on the fixed `PATH`.** Every exec uses the fixed
  `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, so `go`, `gofmt`, `python`,
  `python3`, `uv` and `uvx` are linked into `/usr/local/bin`.
- **The Go toolchain is fixed.** `GOTOOLCHAIN=local` stops Go from downloading a different
  toolchain. A `go.mod` that needs a newer Go fails loudly, so rebuild the image with a newer
  `GO_VERSION`.
- **The Python interpreter is fixed.** `UV_PYTHON_DOWNLOADS=never` means uv uses the baked-in
  Python 3.12. Python itself is marked externally managed (PEP 668), so use `uv venv`, `uv sync`
  or `uv run`, or `python -m venv`, not a global `pip install`.
- **Git accepts any repository owner.** `git config --system safe.directory '*'` stops git from
  refusing a bind-mounted checkout that belongs to a different uid.

## Checking the image

This check was run against this image with the Go + Python + npm allow-list above. Every command
succeeded:

- `go mod tidy && go build` fetched `golang.org/x/text` through `proxy.golang.org` and
  `sum.golang.org`.
- `uv add six && uv run …` succeeded.
- `pnpm add is-number` and `npm install left-pad` succeeded.

`curl https://github.com` was refused with `CONNECT tunnel failed, response 403`.

## Limits

- **`/tmp` is 1 GB.** Large Go builds or module caches can fill it. Raise the tmpfs size, or
  accept that caches are rebuilt for each session.
- **The proxy decides by host name only.** It can't tell one package on an allowed host from
  another, because it doesn't intercept TLS. A registry that serves content from arbitrary users,
  such as `github.com` or `raw.githubusercontent.com`, can also be used to pull or push arbitrary
  data. Allow those hosts deliberately.
- **Only HTTP and HTTPS are proxied.** SSH (`git@github.com:…`) and other protocols have no route.
  Use HTTPS remotes.
