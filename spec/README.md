# spec/ — language-neutral conformance cases

LHA has two implementations (`python/` and `go/`) that must behave identically wherever
behaviour is observable: which commands a human must approve, which URLs may be fetched, what
gets redacted, how checklists move, what bytes a hash chain covers, what a model call costs. The
JSON files here pin that behaviour, and both test suites run them. Go gains each file's runner as
the matching package is ported; it runs every file below except
`coordination/shared_paths.json`, because file ownership is not ported yet.

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
| `model/pricing.json` | Claude price table and per-call cost |

## Changing behaviour

The Python implementation is the reference. To change a behaviour on purpose: change Python,
regenerate with `cd python && uv run python scripts/export_spec.py`, review the JSON diff, then
make Go pass the new cases. A case is never edited to match a bug.
