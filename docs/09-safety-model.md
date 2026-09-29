# Safety model

Safety is enforced in code below the model, never through a prompt. There are several layers:

1. the sandbox the code runs in;
2. the tool dispatcher every tool call passes through;
3. a command classifier that routes irreversible commands to a human gate;
4. an egress policy for in-process HTTP, and an allow-list proxy for the sandbox's network;
5. a Rule-of-Two capability check;
6. secret hygiene in child processes and in traces;
7. a hardened host-side git that nothing in the work tree can make run code.

Each layer assumes the others can fail.

Code: [`python/src/lha/safety/`](../python/src/lha/safety/),
[`python/src/lha/execution/`](../python/src/lha/execution/),
[`python/src/lha/hitl/`](../python/src/lha/hitl/). The Go port has equivalents of the classifier
and egress checks in `go/internal/safety/`, which run the same `spec/` cases, the local and Docker
sandboxes, egress proxy, dispatcher and tools in `go/internal/execution/`, and the console
approval gate in `go/internal/hitl/`, and its durable (Temporal) gates in `go/internal/durable/`;
it has no E2B sandbox.

## 1. Sandboxes

`build_sandbox()` / `open_sandbox()` ([factory.py](../python/src/lha/execution/factory.py)) are
the single entry point. `LHA_SANDBOX` selects the kind. The default is `docker`.

| Kind | Isolation | Notes |
|---|---|---|
| `docker` | container | Needs the `sandbox` extra (`docker>=7.1`). Image `LHA_SANDBOX_IMAGE`, default `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`; [`sandbox/Dockerfile`](../sandbox/Dockerfile) builds a Go + uv + Node/pnpm image |
| `e2b` | Firecracker microVM (E2B service) | Needs the `e2b` extra (`e2b-code-interpreter`). The workspace is synced both ways (below); tested against a fake SDK only, not against the E2B service in CI. Python only |
| `local` | none | Refused with `UnsafeSandboxError` unless `LHA_ALLOW_UNSAFE_LOCAL=true` or `--unsafe-local` |

Docker hardening, from `DockerSandbox.run_kwargs()` in
[sandbox_docker.py](../python/src/lha/execution/sandbox_docker.py):

- `network_mode="none"` unless the sandbox egress settings list hosts (then only those hosts, through
  the egress proxy; see [sandbox network](#sandbox-network)) or the sandbox is built with `network=True` (full
  network; no entry point passes it, and it cannot be combined with an allow-list). This is where
  egress for `run_command` is actually blocked.
- `mem_limit` and `memswap_limit` `LHA_SANDBOX_MEMORY` (default 2g), `pids_limit` 512,
  `nano_cpus` for `LHA_SANDBOX_CPUS` CPUs (default 2).
- `cap_drop=["ALL"]`, `security_opt=["no-new-privileges:true"]`, and `init=True` (an init
  process as PID 1 reaps orphaned processes, so killing a command's process group on timeout
  leaves no zombies).
- A non-root user: the host `uid:gid` on POSIX, otherwise `65534:65534`.
- `read_only=True` root filesystem, `tmpfs /tmp` (`rw,exec,nosuid,nodev`, size `LHA_SANDBOX_TMP_SIZE`, default 1g, counted
  against the memory limit). `/tmp` is
  mounted `exec` because toolchains such as `go test` build and run binaries there; code can
  already run from `/workspace`, so this grants nothing new.
- The host workdir is bind-mounted read-write at `/workspace`. Its `.git` and `.lha`, if present,
  are re-mounted read-only whether they are directories or files, so code in the container cannot
  plant hooks or rewrite mission state. In a linked git worktree (every parallel implementer's, or
  a mission workdir that is one) `.git` is a `gitdir: <path>` file; mounted read-only it cannot be
  repointed at a repository the agent planted. The git dir it names normally lies outside the
  mount (`<repo>/.git/worktrees/<name>`); if it or its common dir lies inside the work tree, it is
  bound read-only too (and the host refuses that layout, see [section 7](#7-host-side-git)).
- Every exec is wrapped in `timeout -k 5 <n>s`, with a host-side deadline of `n + 30` s. Each
  output stream is kept in a bounded head+tail buffer (1 MB per stream). The environment is a
  fixed minimal set (`PATH`, `HOME=/workspace`, locale, `TMPDIR`, `GIT_TERMINAL_PROMPT=0`).
- File paths are contained lexically, then re-checked with `realpath -m` inside the container.

`local` runs commands as the host user with host network access. It confines the file tools to the
workdir, and it gives children a minimal environment (see section 6). It does not restrict what a
command itself can read, write or reach.

`e2b` works in its own VM filesystem (`/home/user/workspace`), so
[sandbox_e2b.py](../python/src/lha/execution/sandbox_e2b.py) keeps it in step with the host
workdir, which stays the source of truth (the git anchor, harness integrity, trusted checks and
the checkpoint all read the host):

- On `open`, and before every `exec`, `write_file` and `read_file`, host changes are sent in:
  the files git would commit (`git ls-files -co --exclude-standard`; every file outside a git work
  tree), as one tar. `.git`, `.lha`, any `.git` path component and ignored files (`.env`,
  `.venv`...) are never sent. After the first upload only what changed on the host is sent, and
  host deletions are replayed, so a failed attempt the harness rolled back is rolled back in the
  VM too.
- After every `exec` and `write_file`, the VM's changes come back: new and changed files
  (by size, mtime, ctime and mode), except paths the host's ignore rules ignore, and deletions of
  files the session mirrors. So the checks the verifier runs in the VM and the tree the checkpoint
  commits are the same files.
- Copy-back is the trust boundary. It uses `tarfile`'s `data` filter (no absolute or escaping
  paths or links, no device files), accepts only the members it asked for, refuses anything
  under `.git`/`.lha`, never writes through a host symlink, and caps each transfer at 256 MiB.
- Every failure fails closed: an `exec` whose sync did not complete returns exit code `-1`, and a
  command whose exit code the SDK does not report is also `-1`, never a pass.
- No snapshots: `snapshot()` and `open(snapshot_id=...)` raise `NotImplementedError`; a resumed
  mission opens a new VM from the host checkout.

The VM template needs `python3` (3.8 or later) for the sync helper. The Go implementation has no
E2B client and refuses `e2b` with an error.

## 2. The tool dispatcher

Every tool call goes through `AllowListDispatcher`
([dispatcher.py](../python/src/lha/execution/dispatcher.py)). It returns failures as `ToolResult`s
and never raises into the agent loop. Checks run in this order:

1. **Allow-list.** The tool must be registered and named in `allow`. An empty `allow` permits
   nothing. `for_tools()` allows every tool it is given.
2. **Mutating flag.** Tools with `mutating=True` are refused unless `allow_mutating`.
3. **Egress flag.** Tools with `egress=True` are refused unless `allow_egress` (default-deny).
4. **Argument validation** against the tool's JSON Schema subset: required keys, `type` (a `bool`
   is never an integer or number), `properties`, `additionalProperties`, `items`, `enum`,
   `minimum` and `maximum`.
5. **Path containment** for each argument listed in `path_args`. The path must stay inside the
   workspace: absolute paths, drive letters, NUL bytes, `..` escapes and symlink escapes are
   rejected ([paths.py](../python/src/lha/execution/paths.py)). A mutating tool may not target
   `.git/` or `.lha/`, compared case-insensitively.
6. **Command gate.** If the tool declares a `command_arg` (only `run_command` does), the argv is
   classified. A gated command is sent to the configured `HITLGate` with its tool, arguments,
   reason and fingerprint. With no gate configured it is denied. Every answer (approve, reject,
   queued, gate error, no gate) is kept as a `tool_approval` event and committed with the cycle's
   checkpoint.

The default toolset (`default_local_tools()`) is `read_file`, `write_file` (mutating),
`edit_file` (mutating: replaces one exact snippet, refused unless it matches exactly once),
`list_files`, `grep` and `run_command` (mutating, `command_arg="argv"`, no shell). Every run path
builds its dispatcher with `build_run_dispatcher()`
([toolset.py](../python/src/lha/execution/tools/toolset.py)); the lead's goes through
`lead_dispatcher()` in [agent/assembly.py](../python/src/lha/agent/assembly.py). With
`LHA_CODE_QUERY=true` every role also gets `code_query`: read-only, no egress, running ripwire in
the sandbox with the question's target as one argument. When
`LHA_WEB_ALLOW_HOSTS` is set it adds `fetch_url` (and `web_search`, when configured) and allows
egress for them (section 4). `SubAgent` applies its role's mutating and
egress policy a second time, hiding and refusing tools the role may not use, even when the
dispatcher would allow them.

Which gate the Lead's dispatcher gets depends on the run path; see
[Human gates on tool calls](#human-gates-on-tool-calls) below.

## 3. Command classifier and human gates

`classify_command(argv)` ([commands.py](../python/src/lha/safety/commands.py)) returns a reason
string if a human must decide, otherwise `None`. The dispatcher also passes where the command
runs: the session's workdir (`/workspace` in Docker) and whether `/tmp` belongs to the sandbox,
which is true when the workdir is not the host checkout (Docker, E2B) and never for `local`. What it gates:

| Category | Examples |
|---|---|
| Publish / push | `git push`, `send-pack`, `update-ref`, `remote-*`; `npm/pnpm/yarn/bun/uv/poetry/hatch/flit/pdm publish`, `twine upload`, `cargo publish`, `gem push`, `docker/podman push/login`, `mvn deploy`; `npm run <publish|deploy|release script>` |
| Destructive git | `reset --hard`, `clean -f…`, `branch -D` / `-d -f`, `filter-branch`, `filter-repo`, `reflog expire`, `commit/rebase --amend|--root`, `checkout/switch/restore --force`, `remote add/set-url/remove` |
| Git config that runs code or changes remotes | `git config` or `git -c` / `--config-env` keys `alias.*`, `include*`, `core.hookspath`, `core.sshcommand`, `core.pager` (except `git -c core.pager=cat`, `less` or `more`), `core.editor`, `credential.*`, `remote.*`, `url.*`, `*.pushurl`, `filter.*`, …; `GIT_CONFIG_*` environment assignments; `--upload-pack`, `--receive-pack`, `--exec=`, `ext::` transports |
| Infra and deploy CLIs | always gated: `aws`, `gcloud`, `gsutil`, `az`, `heroku`, `gh`, `glab`, `ansible*`; by subcommand: `kubectl`, `helm`, `terraform`, `tofu`, `pulumi`, `fly`, `vercel`, `netlify`, `firebase`, `serverless`, `wrangler` |
| Uploads | `curl -d/-F/-T/--json/--data*/--form*`, `-X/--request POST|PUT|PATCH|DELETE`, `-K/--config`; `wget --post-*`, `--body-*`, `--method` writes, `-e` upload settings; httpie/xh with a body or a write method; `rsync` to `host:` |
| Remote shells and mail | `ssh`, `scp`, `sftp`, `ftp`, `telnet`, `nc`/`ncat`/`netcat`, `socat`, `sendmail`, `mail`, `mailx` |
| System | `shutdown`, `reboot`, `halt`, `mkfs`, `dd` |
| Deletes and protected paths | `rm` of absolute, `~`, `..`, `.` or `*` targets (an absolute path inside the workspace is read relative to it, and in Docker or E2B a path under `/tmp` is the sandbox's own, not outside); `rm`, `mv`, `cp`, `ln`, `chmod`, `chown`, `touch`, `tee`, `truncate`, `shred` or `sed -i` on `.git`/`.lha`; `find … -delete` on them; shell redirections (`>`, `>>`, `>\|`, `&>`, `2>`, `<>`) into them. In `sh -c` scripts, redirection targets are also checked in the raw script text, so a clobbering `>\|` (which the tokenizer splits into `>` and a pipe) is caught |
| Privilege escalation | `sudo`, `doas`, `su`, `pkexec` |

To find the real command, it unwraps:

- launchers: `env`, `nohup`, `nice`, `time`, `command` (not `command -v` / `-V`, which only
  looks a name up), `exec`, `stdbuf`, `ionice`, `timeout`,
  `xargs`, `npx`, `bunx`, `setsid`, `flock`, `taskset`, `chrt`, `unbuffer`, `caffeinate`,
  `watch`, `script`, `uvx`;
- runners: `uv`/`poetry`/`pipenv`/`pdm`/`hatch`/`pipx run`, `pnpm`/`yarn exec|dlx`,
  `bundle exec`, and `python -m`;
- `find -exec/-execdir/-ok/-okdir`;
- `sh`/`bash`/`zsh`/`dash`/`ksh`/`fish -c` and `eval` scripts, split on `; && || | & ( )` and
  newlines, including `$(…)`, backticks and process substitution. A `$(…)` used as an argument
  (`echo $(date) $(cat f)`, `for i in $(seq 3)`) is classified by its body and then skipped, unless
  the script contains a backslash.

It fails closed in these cases:

- An unknown launcher option is read both as a flag and as value-taking, and both readings are
  classified.
- More than 64 readings, or more than 8 nested launchers, is "ambiguous".
- An unparseable script or unterminated quote is gated.
- Shell nesting deeper than 3 is gated.
- A command name or git subcommand that contains `$` is gated, including a `$(…)` in command
  position (`$(echo rm) -rf /`).

The pinned behaviour is [`spec/safety/classify_command.json`](../spec/safety/classify_command.json).
It has 224 cases of argv mapped to an exact reason, 70 of which are allowed (`null`), plus 16
`scoped_cases` classified with a workspace path and a private `/tmp`. Both
`python/tests/unit/test_spec_conformance.py` and `go/internal/spec/conformance_safety_test.go`
run it. The Python code is the reference. To change behaviour, change Python, regenerate with
`scripts/export_spec.py`, then make Go pass ([spec/README.md](../spec/README.md)).

### Human gates on tool calls

The dispatcher's requests are `IRREVERSIBLE` with default `reject`. They carry the tool, its
arguments, the exact argv (for `run_command`), the classifier's reason and a fingerprint of the
tool and its exact arguments. The gate depends on the run path
([hitl/approvals.py](../python/src/lha/hitl/approvals.py)):

| Run path | Gate | Behaviour |
|---|---|---|
| `lha run-local` / `mission` / `orchestrate` without `--approve-interactive` | none | every flagged command is denied |
| same, with `--approve-interactive` | `TerminalApprover` (`console_gate()`) | Prints the tool, the exact argv (shell-quoted), the reason and the timeout, then asks `Allow this exact call? [y/N]`. Only `y`/`yes` approves; any other answer, end of input, or no answer within `LHA_CONSOLE_APPROVAL_TIMEOUT_S` (default 3600) rejects. Reminders at `LHA_GATE_ESCALATION_SECONDS` are printed, recorded as `gate_reminder` events and sent to the optional webhook. When stdin is not a TTY it rejects without asking. One prompt at a time. |
| durable mission (Temporal) | `DeferredApprovalGate` | Never blocks: an unapproved call is refused for now and queued; after the cycle the workflow opens a gate (status `WAITING_ON_HUMAN`, default **reject**, escalation ladder). An approval allows that exact fingerprint once in a later cycle; a rejection is remembered and never asked again. See [Durable execution](08-durable-execution.md#irreversible-tool-calls). |

A gate can only let through a call the classifier flagged and a human explicitly approved; every
timeout, error, missing terminal and unanswerable prompt rejects. Decisions are committed to
`.lha/events.ndjson` as `tool_approval` events (tool, arguments with secrets redacted, reason,
fingerprint, `decision`, `approved`, `resolved_by`, `defaulted`).

The generic policies are in [hitl/gate.py](../python/src/lha/hitl/gate.py). `AutoPolicyGate`
auto-approves reversible requests and applies the default to irreversible ones. `CallbackGate`
asks an async resolver, and falls back to the default on `None`, on an exception, or on an answer
outside the offered options. No CLI run path uses them.

## 4. Egress policy

There are two egress controls: a policy for in-process HTTP (`fetch_url` and `lha vendor`), and
a proxy for the sandbox's own network.

### In-process HTTP

The egress policy is implemented in [egress.py](../python/src/lha/safety/egress.py) and applies to
`fetch_url` and to `lha vendor`:

- **Default-deny allow-list.** A URL passes only if its scheme is http/https and in
  `allow_schemes`, its normalized host is in `allow_hosts`, and its port is the scheme default or
  in `allow_ports`. Userinfo in the URL is refused. `FetchUrlTool` with no policy denies
  everything.
- **Host normalization.** Lower-case, strip brackets and a trailing dot, then IDNA 2008 + UTS#46
  via the `idna` package (the encoder httpx uses). A host that cannot be encoded is invalid.
  `is_ambiguous_idn()` refuses hosts whose IDNA 2003 and IDNA 2008 encodings differ (`ß`, `ς`,
  ZWJ…).
- **Public-address check.** Every resolved address must be globally routable unicast.
  IPv4-mapped, 6to4 and Teredo addresses are unwrapped first. IP literals are checked directly.
  Deprecated IPv6 site-local addresses (`fec0::/10`) count as private, because some networks
  still route them internally.
- **Redirects.** Redirects are not auto-followed. Each hop (at most 5) is re-checked against the
  policy and re-resolved. The request's actual host and port must equal the checked ones. The tool
  ignores host proxy and netrc settings (`trust_env=False`), rejects `Host`/`Proxy-*` headers, and
  caps the body at 2 MB.
- **Pinned connections (DNS rebinding).** `fetch_url`, `web_search` and `lha vendor` resolve
  each hop's host once, check every address, and connect only to one of those addresses
  ([pinned_http.py](../python/src/lha/safety/pinned_http.py)): a `PinnedNetworkBackend` under
  httpx dials the pinned IP instead of the name, and never resolves anything itself (an
  unpinned host or a non-public IP literal is refused). The `Host` header, the TLS SNI and the
  certificate check still use the hostname. A server that answers the check with a public
  address and a later lookup with `127.0.0.1` cannot move the connection
  (`tests/unit/test_web_pinning.py`).
- **Credential broker.** `CredentialBroker` maps placeholder tokens to real secrets bound to one
  or more hosts. `resolve_headers()` substitutes a secret only into requests to a bound host, so
  the agent sees only placeholders.

`lha vendor` builds its allow-list from the hosts of the URLs it is given. Egress cases are
pinned in [`spec/safety/egress.json`](../spec/safety/egress.json).

### Web tools

The web tools are off unless the operator lists hosts. `build_run_dispatcher()` in
[toolset.py](../python/src/lha/execution/tools/toolset.py) is the only place run paths assemble
tools: `lha mission`, `lha run-local`, `lha orchestrate` and the Temporal cycle activity
(`run_agent_cycle`) reach it through `lead_tools()` / `lead_dispatcher()` in
[agent/assembly.py](../python/src/lha/agent/assembly.py) (so do the parallel implementers, in
`orchestrate` and in the durable `run_implementer` activity); the orchestrator's Researchers and
Reviewer, the sub-agent activity (`run_subagent`) and the durable `review_cycle` activity (with
`allow_egress=False`) call it directly.

| Setting | Effect |
|---|---|
| `LHA_WEB_ALLOW_HOSTS` (comma-separated) | empty (default): no web tools are registered. Non-empty: `fetch_url` is registered and may reach exactly these hosts |
| `--allow-host HOST` (repeatable) | on `mission`, `run-local` and `orchestrate`, adds hosts for this run to `LHA_WEB_ALLOW_HOSTS`. Durable missions use the worker's settings |
| `LHA_WEB_ALLOW_PORTS` | extra ports beyond 80/443 |
| `LHA_WEB_SEARCH_PROVIDER` + `LHA_WEB_SEARCH_API_KEY` | also registers `web_search` (`tavily` or `exa`), still only with a non-empty allow-list |
| `LHA_WEB_SEARCH_ENDPOINT` | overrides the provider's public endpoint |
| `LHA_WEB_CREDENTIALS` | brokered credentials for `fetch_url` (below) |
| `LHA_WEB_TIMEOUT_S`, `LHA_WEB_MAX_RESPONSE_BYTES` | per-request timeout (30 s) and body cap (2,000,000 bytes) for both tools |

Which agents get them: the Lead in every path; the Researchers in `lha orchestrate`; a durable
sub-agent when both its role (`researcher`) and its `SubAgentInput.allow_egress` allow egress
(the durable organization's researchers set it). The Reviewer's and the implementers' roles hide
them (`SubAgent` role filtering). `allow_egress=False` on
`run_mission_local` / `Orchestrator.run_mission` drops them.

`fetch_url` applies the in-process policy above with `allow_hosts` = the allow-list (normalized the
same way, so `Bücher.Test` allows `xn--bcher-kva.test`), `allow_ports` = `LHA_WEB_ALLOW_PORTS`,
the body cap `LHA_WEB_MAX_RESPONSE_BYTES` and the timeout `LHA_WEB_TIMEOUT_S`.
`LHA_WEB_CREDENTIALS` is a secret JSON object mapping a placeholder to
`{"value": "<secret>", "hosts": [...]}`; every bound host must be in the allow-list, or the run
refuses to start. The tool description lists the placeholders and their hosts (never the values),
and the broker substitutes a secret only into a request to a bound host, so a redirect to another
allowed host carries the placeholder, not the secret.

`web_search` may reach only its endpoint: the endpoint's scheme, host and port form its own
policy, the host must resolve to public addresses, and the API key is placed into the request by
a `CredentialBroker` bound to the endpoint host. The endpoint host does not need to be in
`LHA_WEB_ALLOW_HOSTS`. A placeholder in the model's query is removed before substitution.

Both tools are `untrusted_input=True`. Their output goes through `mark_untrusted()`
([untrusted.py](../python/src/lha/execution/tools/untrusted.py)): secret-looking strings are
masked with the trace redactor (section 6), and the text is wrapped in
`<untrusted_content source="...">` with a first line telling the model to treat it as data. Fence
tags inside the content are escaped so a page cannot close the fence early. The fence is a prompt
signal; the enforcement is the Rule of Two (section 5).

### Sandbox network

With the three sandbox egress settings below empty (the default) the Docker sandbox has no
network. With any of them set, each sandbox session gets
([sandbox_docker.py](../python/src/lha/execution/sandbox_docker.py),
[egress_proxy.py](../python/src/lha/execution/egress_proxy.py)):

- an `--internal` Docker network `lha-egress-<random>` with no route out, which is the only
  network the sandbox container joins;
- a proxy container `lha-egress-proxy-<random>` (`python:3.12-alpine`, non-root, all capabilities
  dropped, read-only root) on both that network and the default bridge, running the stdlib-only
  `egress_proxy.py`;
- `HTTP_PROXY`/`HTTPS_PROXY` (and lower-case forms) pointing at the proxy, and
  `NO_PROXY=localhost,127.0.0.1`.

The proxy allows a request only if the host equals an entry, or is a subdomain of an entry
written with a leading dot (`.golang.org` allows `golang.org` and its subdomains). IP-literal hosts
are denied. It resolves the host and denies the request if any address is non-public (the same
rules as above), then connects to the address it checked, without re-resolving. Ports 80 and 443
are allowed; another port only through an explicit `host:port` entry. It supports `CONNECT`
(TLS stays end to end, so it sees only the host name) and absolute-URI plain HTTP; everything else
gets `403`. A malformed allow-list is rejected when the run starts. Closing the session, or a
failed open, removes the proxy container and the network.

**Every allow-listed host is reachable for writes by code in the sandbox.** The command
classifier (section 3) gates `git push`, `npm publish`, `curl -T` and the like, but it does not
look inside interpreter one-liners (`python -c`, `node -e`, a test or build script). Without a
network that costs nothing; with an allow-list, such a program can send the workspace to any
allowed host without a human gate. The proxy cannot tell a download from an upload: `CONNECT`
keeps TLS end to end, so it sees only the host name. So the allow-list is split by what a host
accepts ([egress_hosts.py](../python/src/lha/execution/egress_hosts.py), Go
[policy.go](../go/internal/execution/egressproxy/policy.go)), and a host in the wrong list stops
the run before it starts (`lha` exits with code 2: `invalid sandbox egress settings`):

| Setting | May list | Refused |
|---|---|---|
| `LHA_SANDBOX_EGRESS` | package-registry download hosts, exactly: `pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `crates.io`, `static.crates.io`, `index.crates.io` | anything else, a `.domain` entry, a `host:port` entry |
| `LHA_SANDBOX_EGRESS_EXTRA_HOSTS` | any other host (a private mirror, a docs site), `.domain` and `host:port` entries | an entry that reaches a known push/upload host: code hosting (`.github.com`, `.gitlab.com`, `.bitbucket.org`, `.codeberg.org`, `.sr.ht`, Azure DevOps), package upload (`upload.pypi.org`, `.test.pypi.org`), object stores (`.amazonaws.com`, `.blob.core.windows.net`, R2, Spaces, B2), container registries (`.docker.io`, `ghcr.io`, `.pkg.dev`), `.huggingface.co`, paste and webhook services. `.com` is refused because it covers `github.com` |
| `LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS` | hosts you accept code in the sandbox may push or upload to, for example `github.com` for `go get` of a module the Go proxy does not serve | nothing (malformed entries only) |

The lists are a speed bump for the obvious write channels, not a promise that a "package-fetch"
host is read-only:

- `registry.npmjs.org` and `crates.io` accept `publish` from anyone holding a token, including
  one planted in untrusted input for an account the attacker owns;
- `storage.googleapis.com` is a general object store: it accepts writes to any bucket a signed
  URL grants. It is on the list only because `proxy.golang.org` redirects module downloads there
  (leave it out, and vendor Go modules, if that matters to you);
- a request's path can carry data: `proxy.golang.org` fetches an arbitrary module path from its
  origin server, so the path of a module lookup reaches a host of the requester's choosing.

That is why any sandbox egress counts as untrusted content plus external comms under the Rule of
Two (section 5): a run with sandbox egress may not also hold private data.

Every request the proxy decides is recorded. The proxy logs one line per request to stderr
(`docker logs lha-egress-proxy-...`); at each checkpoint the agent loop reads the new lines from
the sandbox session (`drain_egress_events`) and commits them to `.lha/events.ndjson` as
`sandbox_egress` events, one per decision, method, host and port in the cycle, with a `count`
([egress_events.py](../python/src/lha/execution/egress_events.py)). Allowed, denied and failed
requests are all recorded; a plain-HTTP URL is reduced to its host and port. The record is
best-effort (a proxy log that cannot be read leaves the cycle without it); the enforcement is the
proxy.

Only HTTP(S) is proxied, so SSH remotes have no route. See
[`sandbox/README.md`](../sandbox/README.md).

## 5. Rule of two

[rule_of_two.py](../python/src/lha/safety/rule_of_two.py): a session may hold at most two of
untrusted content, private data, and external comms. It is enforced in two places.

**Per run, fail closed.** `check_run_rule_of_two()` in
[toolset.py](../python/src/lha/execution/tools/toolset.py) runs before a run starts (the CLI
checks before planning spends anything; `build_run_dispatcher()` checks again) with the run's
capabilities:

| Capability | Held when |
|---|---|
| untrusted content | the web tools are enabled (non-empty `LHA_WEB_ALLOW_HOSTS`), or the `docker` sandbox has egress (any of `LHA_SANDBOX_EGRESS`, `LHA_SANDBOX_EGRESS_EXTRA_HOSTS`, `LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS`): what it downloads is third-party content the agent reads |
| external comms | the web tools are enabled, or the `docker` sandbox has egress: code in the sandbox can send data to any allowed host ([sandbox network](#sandbox-network)) |
| private data | `LHA_PRIVATE_DATA=true`, or `LHA_SANDBOX=local` (the agent's shell runs on the host, with the host's files, credentials and network) |

So a run with web tools or sandbox egress must use `docker` or `e2b` and must not declare private
data. The sandbox egress settings configure the Docker proxy only, so they add nothing for `local`
or `e2b`. A run that needs packages and holds private data should use an image that already has
its dependencies (or vendor them). Otherwise it
raises `RuleOfTwoViolation`: `lha` exits with code 2 and a message naming the cause, and the
activities raise a non-retryable `ERROR_CONFIG`. A human gate (including the durable
`DeferredApprovalGate`) does not lift this check. It is run-level because the agents of a run
share context (research briefs reach the Lead).

**System One decision models.** A System One endpoint ([25](25-system-one.md)) is called by the
harness, not by the agent: it gets no tool, and its answers can only split or block an item early
or reorder recalled memory, never allow a command or answer a gate. A remote endpoint must be
https, is re-resolved and checked for public addresses on every request, is dialled only at the
vetted addresses, and receives its API key only for its own host. What it is sent is redacted
first. It receives what the lead's model provider already receives, so it adds no Rule of Two
capability. Because it is a second recipient, a run with `LHA_PRIVATE_DATA=true` still refuses a
remote endpoint unless `LHA_SYSTEM_ONE_PRIVATE_DATA_OK=true`. A loopback endpoint (a self-hosted
Kev) is always allowed.

**Per dispatcher.** The dispatcher derives its capability set from its usable tools (`egress`
gives external comms, `untrusted_input` gives untrusted content) plus the capabilities the caller
declares (`build_run_dispatcher()` declares private data as above). Without a gate, holding all
three raises `RuleOfTwoViolation` at construction. With a gate, every egress call goes through the
gate.

## 6. Secrets in child processes and traces

- Child processes started by `run_proc` (the local sandbox and verifier) get an allow-listed
  environment ([proc.py](../python/src/lha/execution/proc.py)): `PATH`, locale, `TZ`, `TERM`,
  `UV_CACHE_DIR`, Windows essentials, plus `HOME` pointing at the workspace. API keys and `LHA_*`
  variables are not inherited. Timeouts are capped at 3600 s, and the whole process group is
  killed on timeout. Trusted checks are the exception: they run on the host with the host
  environment ([verify/trusted.py](../python/src/lha/verify/trusted.py)).
- Settings secrets are `SecretStr`. `lha config` prints `***` for them.
- [obs/redact.py](../python/src/lha/obs/redact.py) masks values under secret-looking keys (not
  `input_tokens`-style counters) and secret-looking strings: `sk-…`, GitHub and Slack tokens, AWS
  key ids, Google keys, `Authorization:` values, `Bearer` tokens and `scheme://user:pass@`. It is
  applied in `TraceRecorder.record()` and to every OTel span attribute and span error status
  (`lha.obs.otel`). It is not a
  structlog processor, so direct `structlog` calls are not redacted. Cases are in
  [`spec/obs/redact.json`](../spec/obs/redact.json).

## 7. Host-side git

The harness runs git on the host in a work tree the agent has just written: `add`, `commit`,
`merge`, `checkout`, `reset`, `clean`, `worktree add`, `show`, `diff` and plumbing
([git_ops.py](../python/src/lha/state/git_ops.py), `go/internal/state/gitops.go`). Nothing the
agent wrote may make that git execute code:

- **Minimal environment.** `PATH` (and `TMPDIR`) only, a private empty `HOME` and
  `XDG_CONFIG_HOME`, `LC_ALL=C`, `GIT_TERMINAL_PROMPT=0`, empty askpass. No operator `GIT_*`
  variable passes through (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`, `GIT_CONFIG_COUNT`/
  `KEY`/`VALUE`, `GIT_CONFIG_PARAMETERS`, ...). The system and global config and the system
  attributes file are not read (`GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_ATTR_NOSYSTEM=1`). The commit identity is the repository's own `user.name`/`user.email`,
  which `init_repo` sets (trusted checks set `GIT_AUTHOR_*`/`GIT_COMMITTER_*` explicitly).
- **Fixed overrides.** Every invocation carries `-c core.hooksPath=/dev/null -c
  core.fsmonitor=false` and empties `core.sshCommand`, `core.gitProxy`, `core.askPass`,
  `credential.helper`, `diff.external`, `gpg.program` and `core.alternateRefsCommand`; the editor
  is `:`, the pager `cat`, signing is off and `protocol.allow=never` (LHA never pushes or
  fetches). A command-line `-c` wins over every config file.
- **Attribute drivers.** `.gitattributes` is agent-writable, but an attribute only *names* a
  driver (`filter=x`, `diff=x`, `merge=x`); the command is *defined* in config
  (`filter.x.clean|smudge|process`, `diff.x.textconv|command`, `merge.x.driver`). The names cannot
  be known in advance, so before every command that can run a driver (everything but plumbing such
  as `rev-parse`, `cat-file`, `for-each-ref`, `update-ref`, `write-tree`) the harness enumerates the
  remaining config (`git config --list --show-origin`, under the same hardened environment) and
  overrides each defined driver with an empty command. An attribute that names an undefined driver
  runs nothing, so no driver runs at all. A `merge=x` file overridden this way merges as a
  conflict, which fails the attempt instead of running the driver.
- **Config from the work tree is refused.** If any config value comes from a file inside the work
  tree, outside its `.git` directory (an `include.path` into it) or under `.git/lha-worktrees/`
  (where implementers write), the git command is refused with a `GitError`.
- **The `.git` pointer is re-validated.** Before every harness git invocation, a `.git` that is a
  file must be a single `gitdir: <path>` line naming an existing git dir outside the work tree.
  For a linked worktree that git dir must sit under `<common>/worktrees/`, its `commondir` must
  lead to that repository and its `gitdir` back-link to this `.git`. A symlinked `.git` is
  refused. Before an implementer's branch is committed (`commit_worktree`), the worktree's common
  dir must also be the mission repository's. Anything else is refused with a `GitError` naming the
  problem before git runs, and the attempt fails closed
  ([git_link.py](../python/src/lha/state/git_link.py), `go/internal/state/gitlink.go`).

The repository's own config (`<repo>/.git/config`, and `config.worktree`) is still read: it is
the operator's, and the Docker sandbox cannot write it. Dropping the global config has
consequences: a filter the operator configured globally (for example Git LFS's `filter.lfs.*`)
does not run, so LFS-tracked files are committed as they are; a `filter.<x>.required` driver
makes the command fail; and `safe.directory` exceptions in `~/.gitconfig` do not apply, so git
refuses a repository owned by another user.

## Residual risks

- The classifier is a guardrail, not a sandbox. An arbitrary program (`python -c …`, a test
  suite, a build script) can do anything its sandbox allows. With `local` that means the host;
  with sandbox egress, it means sending the workspace to any allow-listed host
  ([sandbox network](#sandbox-network)).
- Trusted checks (`LHA_TRUSTED_CHECKS`) run agent-written code outside the sandbox, as the user
  running the mission. They get a minimal environment (no inherited credentials or `LHA_*`
  settings, an empty temporary `HOME`) plus only the names in `LHA_TRUSTED_CHECK_ENV`, but they
  can still read whatever that user can read by absolute path
  ([threat model](07-verification.md#threat-model)).
- An approved action is allowed exactly as it was requested, but the approver sees only the tool
  and its arguments; what a `git push` sends is whatever the agent committed.
- The web tools run in the worker process, not in the sandbox, so sandbox network isolation does
  not cover them; the egress policy and the pinned connections above are their only network
  boundary.
- An allow-listed host can still serve prompt injection. With web tools on, the Lead can act on
  what it read (edit files, run commands in the sandbox). The Rule of Two keeps private data out of
  such runs; it does not make the content safe.
- The Docker container can write anything in the workspace except `.git` and `.lha` (as files or
  directories). The harness checks for weakened test files after the fact
  ([Verification](07-verification.md)). Host-side git ignores everything exec-capable the agent
  could plant in the work tree ([section 7](#7-host-side-git)).
- The `local` sandbox has no mounts: code it runs can rewrite `.git` itself, the repository's
  config and hooks included. Host-side git still refuses a tampered pointer and ignores hooks,
  fsmonitor and drivers, but other repository settings the agent could write there (for example
  `core.worktree`) are only as safe as the host.
- Redaction is pattern-based. Secrets in unrecognized formats pass through.
- The E2B adapter's workspace sync is tested against a fake SDK, not the E2B service. Files a
  background process writes in the VM after a command returns reach the host only with the next
  command's sync (see section 1).
- The `claude_code` lead engine with `LHA_CLAUDE_CODE_TOOLS=native` hands the host workdir to
  Claude Code's own tools. None of the sections above apply to them: only a prefix deny list for
  git history, publishing and web access, which `sh -c` gets around. The default `lha` mode
  serves LHA's tools over MCP instead, so everything above still applies
  ([models](13-models.md#claude_code-claude-code-claude--p)).
