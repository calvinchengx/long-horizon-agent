# Safety model

Safety is enforced in code below the model, never through a prompt. There are several layers:

1. the sandbox the code runs in;
2. the tool dispatcher every tool call passes through;
3. a command classifier that routes irreversible commands to a human gate;
4. an egress policy for in-process HTTP;
5. a Rule-of-Two capability check;
6. secret hygiene in child processes and in traces.

Each layer assumes the others can fail.

Code: [`python/src/lha/safety/`](../python/src/lha/safety/),
[`python/src/lha/execution/`](../python/src/lha/execution/). The Go port has in-progress
equivalents of the classifier and egress checks in `go/internal/safety/`, which run the same
`spec/` cases.

## 1. Sandboxes

`build_sandbox()` / `open_sandbox()` ([factory.py](../python/src/lha/execution/factory.py)) are
the single entry point. `LHA_SANDBOX` selects the kind. The default is `docker`.

| Kind | Isolation | Notes |
|---|---|---|
| `docker` | container | Needs the `sandbox` extra (`docker>=7.1`). Default image `python:3.12-slim` |
| `e2b` | Firecracker microVM (E2B service) | Imports `e2b_code_interpreter`, which no extra provides. Excluded from coverage, not tested in CI |
| `local` | none | Refused with `UnsafeSandboxError` unless `LHA_ALLOW_UNSAFE_LOCAL=true` or `--unsafe-local` |

Docker hardening, from `DockerSandbox.run_kwargs()` in
[sandbox_docker.py](../python/src/lha/execution/sandbox_docker.py):

- `network_mode="none"` unless the sandbox is built with `network=True`. No entry point passes
  `network=True`. This is where egress for `run_command` is actually blocked.
- `mem_limit` and `memswap_limit` 2g, `pids_limit` 512, `nano_cpus` for 2 CPUs.
- `cap_drop=["ALL"]`, `security_opt=["no-new-privileges:true"]`.
- A non-root user: the host `uid:gid` on POSIX, otherwise `65534:65534`.
- `read_only=True` root filesystem, `tmpfs /tmp` (`rw,nosuid,nodev,size=512m`).
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
   classified. A gated command is sent to the configured `HITLGate`. With no gate configured it is
   denied. Every answer (approve, reject, queued, gate error, no gate) is kept as a
   `tool_approval` event and committed with the cycle's checkpoint.

The default toolset (`default_local_tools()`) is `read_file`, `write_file` (mutating),
`list_files`, `grep` and `run_command` (mutating, `command_arg="argv"`, no shell). `web_search`
and `fetch_url` exist but no run path registers them. Which gate the Lead's dispatcher gets depends
on the run path; see [Human gates on tool calls](#human-gates-on-tool-calls) below. `SubAgent` applies
its role's mutating and egress policy a second time, hiding and refusing tools the role may not
use, even when the dispatcher would allow them.

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
| Deletes and protected paths | `rm` of absolute, `~`, `..`, `.` or `*` targets; `rm`, `mv`, `cp`, `ln`, `chmod`, `chown`, `touch`, `tee`, `truncate`, `shred` or `sed -i` on `.git`/`.lha`; `find … -delete` on them; shell redirections (`>`, `>>`, `&>`, `2>`, `<>`) into them |
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
It has 201 cases of argv mapped to an exact reason, 62 of which are allowed (`null`). Both
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
| same, with `--approve-interactive` | `TerminalApprover` (`console_gate()`) | Prints the tool, the exact argv (shell-quoted), the reason and the timeout, then asks `Allow this exact call? [y/N]`. Only `y`/`yes` approves; any other answer, end of input, or no answer within `LHA_APPROVAL_TIMEOUT_SECONDS` (default 3600) rejects. Reminders at `LHA_GATE_ESCALATION_SECONDS` are printed, recorded as `gate_reminder` events and sent to the optional webhook. When stdin is not a TTY it rejects without asking. One prompt at a time. |
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

The egress policy is implemented in [egress.py](../python/src/lha/safety/egress.py) and applies to
in-process HTTP (`fetch_url`):

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
- **Redirects.** Redirects are not auto-followed. Each hop (at most 5) is re-checked against the
  policy and re-resolved. The request's actual host and port must equal the checked ones. The tool
  ignores host proxy and netrc settings (`trust_env=False`), rejects `Host`/`Proxy-*` headers, and
  caps the body at 2 MB.
- **Credential broker.** `CredentialBroker` maps placeholder tokens to real secrets bound to one
  or more hosts. `resolve_headers()` substitutes a secret only into requests to a bound host, so
  the agent sees only placeholders.

`web_search` posts to fixed Tavily or Exa endpoints without consulting an `EgressPolicy`.
There is no setting for an egress allow-list. A caller must construct `EgressPolicy` in code.
Egress cases are pinned in [`spec/safety/egress.json`](../spec/safety/egress.json).

## 5. Rule of two

[rule_of_two.py](../python/src/lha/safety/rule_of_two.py): a session may hold at most two of
untrusted content, private data, and external comms. The dispatcher derives its capability set from
its usable tools (`egress` gives external comms, `untrusted_input` gives untrusted content) plus
any `capabilities` the caller declares, such as private data. Without a gate, holding all three
raises `RuleOfTwoViolation` at construction. With a gate, every egress call goes through the gate.

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
- DNS rebinding: `fetch_url` resolves and checks addresses, then httpx resolves again when it
  connects. A 0-TTL rebinding server can race the check. Sandbox network isolation is the
  backstop.
- The Docker container can write anything in the workspace except `.git` and `.lha`. The harness
  checks for weakened test files after the fact ([Verification](07-verification.md)).
- Redaction is pattern-based. Secrets in unrecognized formats pass through.
- E2B isolation is not integrated with the host workdir (see section 1).
