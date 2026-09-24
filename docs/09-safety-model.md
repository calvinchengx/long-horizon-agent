# Safety model

Safety is enforced in code below the model, never through a prompt. There are several layers:

1. the sandbox the code runs in;
2. the tool dispatcher every tool call passes through;
3. a command classifier that routes irreversible commands to a human gate;
4. an egress policy for in-process HTTP, and an allow-list proxy for the sandbox's network;
5. a Rule-of-Two capability check;
6. secret hygiene in child processes and in traces.

Each layer assumes the others can fail.

Code: [`python/src/lha/safety/`](../python/src/lha/safety/),
[`python/src/lha/execution/`](../python/src/lha/execution/),
[`python/src/lha/hitl/`](../python/src/lha/hitl/). The Go port has equivalents of the classifier
and egress checks in `go/internal/safety/`, which run the same `spec/` cases; it has no
sandboxes, egress proxy or approval gates yet.

## 1. Sandboxes

`build_sandbox()` / `open_sandbox()` ([factory.py](../python/src/lha/execution/factory.py)) are
the single entry point. `LHA_SANDBOX` selects the kind. The default is `docker`.

| Kind | Isolation | Notes |
|---|---|---|
| `docker` | container | Needs the `sandbox` extra (`docker>=7.1`). Image `LHA_SANDBOX_IMAGE`, default `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`; [`sandbox/Dockerfile`](../sandbox/Dockerfile) builds a Go + uv + Node/pnpm image |
| `e2b` | Firecracker microVM (E2B service) | Imports `e2b_code_interpreter`, which no extra provides. Excluded from coverage, not tested in CI |
| `local` | none | Refused with `UnsafeSandboxError` unless `LHA_ALLOW_UNSAFE_LOCAL=true` or `--unsafe-local` |

Docker hardening, from `DockerSandbox.run_kwargs()` in
[sandbox_docker.py](../python/src/lha/execution/sandbox_docker.py):

- `network_mode="none"` unless `LHA_SANDBOX_EGRESS` lists hosts (then only those hosts, through
  the egress proxy; see [sandbox network](#sandbox-network)) or the sandbox is built with `network=True` (full
  network; no entry point passes it, and it cannot be combined with an allow-list). This is where
  egress for `run_command` is actually blocked.
- `mem_limit` and `memswap_limit` 2g, `pids_limit` 512, `nano_cpus` for 2 CPUs.
- `cap_drop=["ALL"]`, `security_opt=["no-new-privileges:true"]`.
- A non-root user: the host `uid:gid` on POSIX, otherwise `65534:65534`.
- `read_only=True` root filesystem, `tmpfs /tmp` (`rw,exec,nosuid,nodev,size=1g`). `/tmp` is
  mounted `exec` because toolchains such as `go test` build and run binaries there; code can
  already run from `/workspace`, so this grants nothing new.
- The host workdir is bind-mounted read-write at `/workspace`. Its `.git` and `.lha` directories,
  if present, are re-mounted read-only, so code in the container cannot plant hooks or rewrite
  mission state.
- Every exec is wrapped in `timeout -k 5 <n>s`, with a host-side deadline of `n + 30` s. Each
  output stream is kept in a bounded head+tail buffer (1 MB per stream). The environment is a
  fixed minimal set (`PATH`, `HOME=/workspace`, locale, `TMPDIR`, `GIT_TERMINAL_PROMPT=0`).
- File paths are contained lexically, then re-checked with `realpath -m` inside the container.

`local` runs commands as the host user with host network access. It confines the file tools to the
workdir, and it gives children a minimal environment (see section 6). It does not restrict what a
command itself can read, write or reach.

`e2b` works in its own VM filesystem (`/home/user/workspace`). Nothing in the adapter copies the
host workdir into the VM or back. The git anchor and harness checks on the host therefore do not
see edits made inside E2B. Treat this adapter as unfinished.

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
`list_files`, `grep` and `run_command` (mutating, `command_arg="argv"`, no shell). Every run path
builds its dispatcher with `build_run_dispatcher()`
([toolset.py](../python/src/lha/execution/tools/toolset.py)); the lead's goes through
`lead_dispatcher()` in [agent/assembly.py](../python/src/lha/agent/assembly.py). When
`LHA_WEB_ALLOW_HOSTS` is set it adds `fetch_url` (and `web_search`, when configured) and allows
egress for them (section 4). `SubAgent` applies its role's mutating and
egress policy a second time, hiding and refusing tools the role may not use, even when the
dispatcher would allow them.

Which gate the Lead's dispatcher gets depends on the run path; see
[Human gates on tool calls](#human-gates-on-tool-calls) below.

## 3. Command classifier and human gates

`classify_command(argv)` ([commands.py](../python/src/lha/safety/commands.py)) returns a reason
string if a human must decide, otherwise `None`. What it gates:

| Category | Examples |
|---|---|
| Publish / push | `git push`, `send-pack`, `update-ref`, `remote-*`; `npm/pnpm/yarn/bun/uv/poetry/hatch/flit/pdm publish`, `twine upload`, `cargo publish`, `gem push`, `docker/podman push/login`, `mvn deploy`; `npm run <publish|deploy|release script>` |
| Destructive git | `reset --hard`, `clean -f…`, `branch -D` / `-d -f`, `filter-branch`, `filter-repo`, `reflog expire`, `commit/rebase --amend|--root`, `checkout/switch/restore --force`, `remote add/set-url/remove` |
| Git config that runs code or changes remotes | `git config` or `git -c` / `--config-env` keys `alias.*`, `include*`, `core.hookspath`, `core.sshcommand`, `core.pager`, `core.editor`, `credential.*`, `remote.*`, `url.*`, `*.pushurl`, `filter.*`, …; `GIT_CONFIG_*` environment assignments; `--upload-pack`, `--receive-pack`, `--exec=`, `ext::` transports |
| Infra and deploy CLIs | always gated: `aws`, `gcloud`, `gsutil`, `az`, `heroku`, `gh`, `glab`, `ansible*`; by subcommand: `kubectl`, `helm`, `terraform`, `tofu`, `pulumi`, `fly`, `vercel`, `netlify`, `firebase`, `serverless`, `wrangler` |
| Uploads | `curl -d/-F/-T/--json/--data*/--form*`, `-X/--request POST|PUT|PATCH|DELETE`, `-K/--config`; `wget --post-*`, `--body-*`, `--method` writes, `-e` upload settings; httpie/xh with a body or a write method; `rsync` to `host:` |
| Remote shells and mail | `ssh`, `scp`, `sftp`, `ftp`, `telnet`, `nc`/`ncat`/`netcat`, `socat`, `sendmail`, `mail`, `mailx` |
| System | `shutdown`, `reboot`, `halt`, `mkfs`, `dd` |
| Deletes and protected paths | `rm` of absolute, `~`, `..`, `.` or `*` targets; `rm`, `mv`, `cp`, `ln`, `chmod`, `chown`, `touch`, `tee`, `truncate`, `shred` or `sed -i` on `.git`/`.lha`; `find … -delete` on them; shell redirections (`>`, `>>`, `>\|`, `&>`, `2>`, `<>`) into them. In `sh -c` scripts, redirection targets are also checked in the raw script text, so a clobbering `>\|` (which the tokenizer splits into `>` and a pipe) is caught |
| Privilege escalation | `sudo`, `doas`, `su`, `pkexec` |

To find the real command, it unwraps:

- launchers: `env`, `nohup`, `nice`, `time`, `command`, `exec`, `stdbuf`, `ionice`, `timeout`,
  `xargs`, `npx`, `bunx`, `setsid`, `flock`, `taskset`, `chrt`, `unbuffer`, `caffeinate`,
  `watch`, `script`, `uvx`;
- runners: `uv`/`poetry`/`pipenv`/`pdm`/`hatch`/`pipx run`, `pnpm`/`yarn exec|dlx`,
  `bundle exec`, and `python -m`;
- `find -exec/-execdir/-ok/-okdir`;
- `sh`/`bash`/`zsh`/`dash`/`ksh`/`fish -c` and `eval` scripts, split on `; && || | & ( )` and
  newlines, including `$(…)`, backticks and process substitution.

It fails closed in these cases:

- An unknown launcher option is read both as a flag and as value-taking, and both readings are
  classified.
- More than 64 readings, or more than 8 nested launchers, is "ambiguous".
- An unparseable script or unterminated quote is gated.
- Shell nesting deeper than 3 is gated.
- A command name or git subcommand that contains `$` is gated.

The pinned behaviour is [`spec/safety/classify_command.json`](../spec/safety/classify_command.json).
It has 206 cases of argv mapped to an exact reason, 64 of which are allowed (`null`). Both
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
[agent/assembly.py](../python/src/lha/agent/assembly.py); the orchestrator's Researchers and
Reviewer and the sub-agent activity (`run_subagent`) call it directly.

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
sub-agent when both its role (`researcher`) and its `SubAgentInput.allow_egress` allow egress. The
Reviewer's role hides them (`SubAgent` role filtering). `allow_egress=False` on
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

With `LHA_SANDBOX_EGRESS` empty (the default) the Docker sandbox has no network. With a
comma-separated list of hosts, each sandbox session gets
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
gets `403`. It logs one line per request to stderr (`docker logs lha-egress-proxy-...`). A
malformed allow-list is rejected when the sandbox is built. Closing the session, or a failed
open, removes the proxy container and the network.

The proxy decides by host name only. A host that serves arbitrary users' content (`github.com`,
`raw.githubusercontent.com`) can be used to pull or push arbitrary data; allow such hosts
deliberately. Only HTTP(S) is proxied, so SSH remotes have no route. See
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
| untrusted content | the web tools are enabled (non-empty `LHA_WEB_ALLOW_HOSTS`) |
| external comms | the web tools are enabled |
| private data | `LHA_PRIVATE_DATA=true`, or `LHA_SANDBOX=local` (the agent's shell runs on the host, with the host's files, credentials and network) |

So a run with web tools must use `docker` or `e2b` and must not declare private data. Otherwise it
raises `RuleOfTwoViolation`: `lha` exits with code 2 and a message naming the cause, and the
activities raise a non-retryable `ERROR_CONFIG`. A human gate (including the durable
`DeferredApprovalGate`) does not lift this check. It is run-level because the agents of a run
share context (research briefs reach the Lead).

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
  killed on timeout.
- Settings secrets are `SecretStr`. `lha config` prints `***` for them.
- [obs/redact.py](../python/src/lha/obs/redact.py) masks values under secret-looking keys (not
  `input_tokens`-style counters) and secret-looking strings: `sk-…`, GitHub and Slack tokens, AWS
  key ids, Google keys, `Authorization:` values, `Bearer` tokens and `scheme://user:pass@`. It is
  applied in `TraceRecorder.record()` and to OTel span attributes (`agent_span`). It is not a
  structlog processor, so direct `structlog` calls are not redacted. Cases are in
  [`spec/obs/redact.json`](../spec/obs/redact.json).

## Residual risks

- The classifier is a guardrail, not a sandbox. An arbitrary program (`python -c …`, a test
  suite, a build script) can do anything its sandbox allows. With `local` that means the host.
- Trusted checks (`LHA_TRUSTED_CHECKS`) run agent-written code outside the sandbox, with the
  privileges of the process running the mission ([verification](07-verification.md#trusted-checks)).
- An approved action is allowed exactly as it was requested, but the approver sees only the tool
  and its arguments; what a `git push` sends is whatever the agent committed.
- The web tools run in the worker process, not in the sandbox, so sandbox network isolation does
  not cover them; the egress policy and the pinned connections above are their only network
  boundary.
- An allow-listed host can still serve prompt injection. With web tools on, the Lead can act on
  what it read (edit files, run commands in the sandbox). The Rule of Two keeps private data out of
  such runs; it does not make the content safe.
- The Docker container can write anything in the workspace except `.git` and `.lha`. The harness
  checks for weakened test files after the fact ([Verification](07-verification.md)).
- Redaction is pattern-based. Secrets in unrecognized formats pass through.
- E2B isolation is not integrated with the host workdir (see section 1).
- The `claude_code` lead engine with `LHA_CLAUDE_CODE_TOOLS=native` hands the host workdir to
  Claude Code's own tools. None of the sections above apply to them: only a prefix deny list for
  git history, publishing and web access, which `sh -c` gets around. The default `lha` mode
  serves LHA's tools over MCP instead, so everything above still applies
  ([models](13-models.md#claude_code-claude-code-claude--p)).
