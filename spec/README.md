# spec/ — language-neutral conformance cases

LHA has two implementations (`python/` and `go/`) that must behave identically wherever
behaviour is observable: which commands a human must approve, which URLs may be fetched, what
gets redacted, how checklists move, what bytes a hash chain covers, what a model call costs. The
JSON files here pin that behaviour, and both test suites run them. Go gains each file's runner as
the matching package is ported; it runs every file below.

- Python: `python/tests/unit/test_spec_conformance.py`
- Go: `go/internal/spec/conformance_*_test.go`

| File | Pins |
|---|---|
| `safety/classify_command.json` | argv -> the exact gate reason (or `null` = allowed) |
| `safety/egress.json` | host normalization (IDNA 2008), URL parsing, public-address checks, allow-list |
| `obs/redact.json` | secret redaction in free text and secret-looking keys |
| `contracts/check_names.json` | verification check names derived from argv, and de-duplication |
| `state/checklist.json` | next actionable item, completion, deadlock reasons, status transitions |
| `coordination/decision_chain.json` | the canonical JSON bytes and SHA-256 chain of the decision log |
| `coordination/shared_paths.json` | files only the lead engineer may write |
| `verify/harness_files.json` | test/harness files the agent may not weaken |
| `verify/flaky_retry.json` | flaky-check re-runs and quarantine: re-run order, verdicts, output tails, and the `check_quarantined` / `quarantined_check_failed` event payloads |
| `model/pricing.json` | Claude price table and per-call cost |
| `model/fallback_models.json` | `LHA_FALLBACK_MODELS` entries -> backend, model and price, or the exact error |
| `state/vendor_paths.json` | where `lha vendor` stores a fetched URL (`<host>/<path>`, query hash, `.html`) |
| `agent/prompts.json` | the lead's system/user prompts byte for byte, the JSON reply protocol, the memory block, and the Planner / Replanner prompts, plan parsing and file ownership |
| `agent/org.json` | the organization: role chart and per-role models, sub-agent prompts and tool visibility, the Reviewer's prompt and verdict parsing, reflection, the implementer's objective, ownership-guard refusals, lease decisions and the ticket lifecycle |

| `execution/paths.json` | workspace path containment: normalization, harness-owned paths, container joins |
| `execution/arguments.json` | tool-argument JSON-Schema validation errors and missing required arguments |
| `execution/sandbox_egress.json` | the sandbox egress lists (package-fetch, extra and write hosts) and their errors, the run-level Rule of Two with sandbox egress, and `sandbox_egress` events parsed from the proxy log |

## Changing behaviour

The Python implementation is the reference. To change a behaviour on purpose: change Python,
regenerate with `cd python && uv run python scripts/export_spec.py`, review the JSON diff, then
make Go pass the new cases. A case is never edited to match a bug.
