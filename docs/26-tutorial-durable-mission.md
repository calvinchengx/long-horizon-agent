# Tutorial: a durable mission, end to end

This tutorial runs a small mission the way LHA is meant to run a large one. A local model builds
and publishes a tiny Python package from a roadmap. It works in a Docker sandbox that can reach
PyPI and nothing else, on a Temporal worker that you crash halfway through, and it has to ask you
before it pushes. Afterwards you read back what happened from git, the mission anchor and the
mission store.

Everything below is from a real run on 27 September 2026 with `gemma4` on Ollama. It took about
five minutes and cost $0. Your model's answers will differ, and a smaller model may need more
attempts; the verifier decides what is done either way.

You need: `git`, [uv](https://docs.astral.sh/uv/), Docker, the
[Temporal CLI](https://docs.temporal.io/cli) and [Ollama](https://ollama.com) with a model pulled
(`ollama pull gemma4`, or `qwen3:8b`). Commands run from a checkout of this repository unless
they say otherwise.

## 1. Build the sandbox image and start Temporal

The agent runs its commands in a container. The default image has Python and uv but no `git`,
and this mission pushes, so build the reference image, which adds git, Go and Node:

```bash
docker build -t lha-sandbox:dev sandbox
```

In a second terminal, start a local Temporal server and leave it running:

```bash
temporal server start-dev
```

## 2. Create the project and its roadmap

The project is an empty Python package with pytest as a dev dependency, and a bare git
repository inside it that stands in for a remote:

```bash
mkdir -p ~/lha-tutorial/greeter && cd ~/lha-tutorial/greeter
cat > pyproject.toml <<'EOF'
[project]
name = "greeter"
version = "0.1.0"
requires-python = ">=3.12"

[dependency-groups]
dev = ["pytest>=8"]
EOF
printf '.venv/\n__pycache__/\n.remote.git/\n' > .gitignore
git init -b main && git add -A && git commit -m "Start the greeter project"
git init --bare .remote.git && git remote add origin ./.remote.git
```

The roadmap is the plan. Each `## ` section is a phase that depends on the one before it, and
each item names a **witness**: the check that proves that item, on top of the mission's own
checks.

```bash
cat > ~/lha-tutorial/roadmap.md <<'EOF'
# Greeter

A tiny Python package, written, tested and published by a durable mission.

## P0 — the code

- [ ] Create greeter.py with greet(name) returning "Hello, <name>!" and test_greeter.py with a test_greet test (witness: pytest:test_greeter.py::test_greet)

## P1 — publish

- [ ] Push the main branch to the origin remote (witness: cmd:git ls-remote --exit-code origin main)
EOF
```

`pytest:test_greeter.py::test_greet` passes only if that test exists and passes, so the model
cannot satisfy it by writing no tests. `cmd:git ls-remote --exit-code origin main` passes only
when the remote really has the branch.

## 3. Configure and start a worker

In a third terminal, from `python/`:

```bash
export LHA_MODEL_BACKEND=ollama LHA_MODEL_NAME=gemma4
export LHA_SANDBOX=docker LHA_SANDBOX_IMAGE=lha-sandbox:dev
export LHA_SANDBOX_EGRESS=pypi.org,files.pythonhosted.org
export LHA_RESET_KEEP=.remote.git
uv sync --extra sandbox
uv run lha worker
```

- `LHA_SANDBOX_EGRESS` gives the sandbox one way out: an egress proxy that allows these two
  hosts, so `uv run pytest` can install pytest. Every other host is refused.
- `LHA_RESET_KEEP=.remote.git` matters here. Each durable cycle attempt starts from a clean
  checkout (`git clean -ffdx`), which would also delete the ignored stand-in remote. Listing it
  keeps it. Build caches are the usual reason to set this.

## 4. Start the mission

In the terminal you used in step 2 (any terminal works; only the worker needs the settings above):

```bash
cd path/to/long-horizon-agent/python
uv run lha mission-start --title Greeter \
  --checklist ~/lha-tutorial/roadmap.md --workdir ~/lha-tutorial/greeter \
  --no-default-checks --check "uv run pytest -q"
```

```text
started mission mission_bc32a2e8c538 (workflow id: mission:mission_bc32a2e8c538)
```

`--checklist` skips the Planner and imports your roadmap. `--no-default-checks --check "uv run
pytest -q"` replaces the default checks (ruff, ty, pytest) with the one this project has. Every
item must pass it, plus its own witness. `mission-start` returns straight away: the mission now
belongs to Temporal and the worker.

## 5. Crash the worker

While the first cycle is running (the worker logs `memory_recall ... cycle_id=c1`), kill the
worker hard: press Ctrl-\ in its terminal, or `kill -9` its process. The model's partial work is
left in the checkout, uncommitted:

```bash
git -C ~/lha-tutorial/greeter status --short
```

```text
?? greeter.py
?? test_greeter.py
```

Temporal still holds the cycle as a started activity whose heartbeat stopped:

```bash
temporal workflow describe --workflow-id mission:mission_bc32a2e8c538
```

```text
Pending Activities: 1
  State                 Started
  Attempt               1
  MaximumAttempts       5
  LastHeartbeatTime     27 seconds ago
```

`lha mission-status` fails for now (`no poller seen for task queue recently, worker may be
down`). Status is a query that a worker answers, and there is no worker. Start one again with
the same `uv run lha worker`. Within the 2-minute heartbeat timeout Temporal gives the cycle to
the new worker as attempt 2. The attempt resets the checkout to the last commit first, so the
crashed attempt's partial files are discarded rather than committed. In the recorded run the
cycle restarted 100 seconds after the kill and was verified 42 seconds later. The history has
exactly one commit for it.

## 6. Approve the push

For item 02 the model runs `git push origin main`. That command is irreversible, so LHA neither
runs nor refuses it: the call is queued, the cycle fails its witness (nothing was pushed), and
the mission parks for a human:

```bash
uv run lha mission-status mission_bc32a2e8c538
```

```text
status=WAITING_ON_HUMAN cycles=2
gate: tool_call approval-faeb6989ce1e
  question: Mission mission_bc32a2e8c538 wants to run an irreversible action: run_command {'argv': ['git', 'push', 'origin', 'main'], 'timeout_s': 10} (git push (outward-facing / rewrites history)). Approve or reject?
  options: approve | reject  (default on timeout: reject)
  opened: 2026-09-27T06:49:30+00:00  deadline: 2026-09-28T06:49:30+00:00
  reminders sent: 0  next reminder: 2026-09-27T07:04:30+00:00
  pending action: run_command {'argv': ['git', 'push', 'origin', 'main'], 'timeout_s': 10}
  reason: git push (outward-facing / rewrites history)
  fingerprint: faeb6989ce1e962b952cbf36b3205242
```

The gate is durable. You can stop the worker now, restart it tomorrow, and the same question,
deadline and reminder schedule are still there. Unanswered, it sends reminders
(`LHA_GATE_ESCALATION_SECONDS`, and `LHA_GATE_WEBHOOK_URL` if set) and rejects at the deadline.
Approve it:

```bash
uv run lha mission-approve mission_bc32a2e8c538 --decision approve
```

The approval covers that exact call, matched by the fingerprint of the tool and its arguments,
and only once. The next cycle runs the push, the witness passes, and the mission ends:

```text
status=DONE cycles=3
recent gate events:
  2026-09-27T06:49:30+00:00 tool_call gate approval-faeb6989ce1e opened (default reject)
  2026-09-27T06:49:47+00:00 tool_call gate approval-faeb6989ce1e resolved: approve
```

## 7. Read the record

Everything the mission did is in the checkout. Each cycle and each gate event is one commit:

```bash
git -C ~/lha-tutorial/greeter log --oneline
```

```text
04e79c9 lha: complete 02 (Push the main branch to the origin remote)
17074d5 lha: gate resolved (tool_call approval-faeb6989ce1e)
49522b2 lha: gate opened (tool_call approval-faeb6989ce1e)
7c64cd2 lha: attempt 02 (Push the main branch to the origin remote)
3e2b8cb lha: complete 01 (Create greeter.py with greet(name) returning "Hello, <name>!" and test_greeter.py with a test_greet test)
3186d91 lha: initialize mission anchor
4145ff3 Start the greeter project
```

The checklist says what proved each item (`.lha/checklist.json`, abridged):

```text
01 done  attempts 1  verified_by ['pytest', 'pytest:test_greeter.py::test_greet']
02 done  attempts 2  verified_by ['pytest', 'cmd:git ls-remote --exit-code origin main']
```

`.lha/progress.md` has one line per cycle, and `.lha/events.ndjson` records every gated call,
with its decision and who made it (`pending` in cycle 2, `approve` by the `operator` in cycle 3).
The mission store has the outcome and every model call:

```bash
uv run lha missions
uv run lha costs mission_bc32a2e8c538 --limit 0   # totals only; the default also lists the calls
```

```text
mission_bc32a2e8c538  DONE             $0.0000  calls 24  head 04e79c9c192d  updated 2026-09-27T06:50:44  Greeter

total: 24 calls  known $0.0000  unknown-cost calls 0  tokens in 28103 out 8287
```

The model wrote:

```python
def greet(name):
    """Returns a greeting string for the given name."""
    return f"Hello, {name}!"
```

## Clean up

Stop the worker and the Temporal server (Ctrl-C), and remove `~/lha-tutorial`. The mission's
row stays in the mission store; `lha config` prints where that is.

## What to try next

- Give the same roadmap to a durable organization: `--research 1 --review` adds researchers and
  an independent reviewer ([multi-agent organization](11-multi-agent-organization.md)).
- Run the same steps with the Go CLI: `go build -o lha ./cmd/lha` from `go/`. It has the same
  commands and settings. Use one implementation per task queue.
- Scale up: [building a large project](24-large-missions.md) covers trusted checks, protected
  paths, vendored references and replanning.
