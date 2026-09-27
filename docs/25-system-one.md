# System One decision models

A System One model answers typed questions with calibrated probabilities instead of generating
text. You send it a `state` and a few questions: yes/no (`noul`), pick one option (`choice`), or
rate on ordered levels (`score`). It returns a probability for every possible answer in about
100 ms, for a fraction of an LLM call. TypeSafe AI's **Jev** (released 15 September 2026) is the
hosted one. **Kev** is an Apache-2.0 open-weights family that serves the same
`POST /v1/systemone` API, so LHA talks to both with one client.

LHA uses these answers for exactly two things, and only in ways that make it more cautious or
cheaper:

| Use | What is asked | What a confident answer changes |
|---|---|---|
| **Stall triage** | after an item fails twice in a row: *why do the attempts fail?* (a fixable defect, too much scope, or the environment) | `scope`: the item is split now, not after `max_consecutive_failures`. `environment`: the item is blocked now, for a human |
| **Memory reranking** (`LHA_MEMORY_RERANK=system_one`) | per recalled passage: *does it help with this task?* | recalled memory is reordered, and passages below `LHA_SYSTEM_ONE_RERANK_MIN` dropped |

A System One answer never allows a command, never answers a human gate and never marks an item
done. The deterministic verifier and the command classifier keep those decisions. When the
model is down, slow, over budget or returns a malformed answer, LHA behaves exactly as it does
with System One off.

## Why only in that direction

The published evidence, from TypeSafe's own documentation and from independent tests in the two
weeks after Jev's release, says a System One model is fast and never returns a malformed answer,
but is not a security boundary:

- **Injected text moves it.** In one independent test, an action gate's probability of blocking
  `rm -rf ~/.ssh` fell from 0.76 to 0.48 after a fake pre-approval was injected into tool output.
  TypeSafe's list of Jev 1.13's weaknesses says adversarial content "can move the answer".
- **Confidence is not correctness off-distribution.** Jev is well calibrated on familiar English
  classification. Elsewhere it has returned confidence 1.00 on wrong answers, and the direction
  of its calibration error changes by domain.
- **Wording matters.** Bare option labels, or swapping `yes`/`no` names, changed a third of the
  answers in one test. LHA's questions give every option a full, neutral description.

So LHA asks only questions where a wrong answer costs effort, not safety. The worst a wrong
triage answer can do is split an item that did not need it, or send a human an item that one
more attempt would have fixed. The parent's witnesses still gate the last child of a split, so a
split never makes anything pass more easily.

## Choosing a backend

| Backend | Where | Cost | Choose it when |
|---|---|---|---|
| Jev (hosted) | `https://api.typesafe.ai/v1/systemone` | $0.042 per million input tokens, output free | the mission holds no private data and you want the best-calibrated answers |
| Kev-4B / Kev-9B (self-hosted) | `http://127.0.0.1:8009/v1/systemone` | $0 (your GPU or Apple Silicon) | private or offline missions; Kev-0.8B on a laptop |
| Anything else that serves `/v1/systemone` | your endpoint | set `LHA_SYSTEM_ONE_PRICE_IN_PER_MTOK` | e.g. a Laya or Von server |

Kev-4B reports 0.817 accuracy on unseen sources against Jev's 0.857 (its own published
comparison; not an independent benchmark). To run it locally:

```bash
git clone https://github.com/jaredpalmer/kev.git && cd kev
uv sync --extra serve
uv run --extra serve python -m kev.serve --run jaredpalmer/kev-4b --port 8009
```

## Configuration

```bash
# Hosted Jev
export LHA_SYSTEM_ONE_BACKEND=systemone
export LHA_SYSTEM_ONE_API_KEY=...            # a secret; bound to the endpoint host

# ...or a local Kev
export LHA_SYSTEM_ONE_BACKEND=systemone
export LHA_SYSTEM_ONE_ENDPOINT=http://127.0.0.1:8009/v1/systemone
export LHA_SYSTEM_ONE_MODEL=kev-latest

# Optional: rerank recalled memory with it too
export LHA_MEMORY_RERANK=system_one
```

| Variable | Default | Meaning |
|---|---|---|
| `LHA_SYSTEM_ONE_BACKEND` | `off` | `off`, `systemone` (the HTTP API) or `stub` (tests: uniform answers, so nothing ever acts) |
| `LHA_SYSTEM_ONE_ENDPOINT` | TypeSafe's | must be `https` unless it is loopback (`localhost`, `127.0.0.0/8`, `::1`) |
| `LHA_SYSTEM_ONE_API_KEY` | unset | required for a remote endpoint |
| `LHA_SYSTEM_ONE_MODEL` | `jev-1.13.0` | pin a version, not `jev-latest`: an alias moves when a new release ships, and thresholds are tuned per version |
| `LHA_SYSTEM_ONE_TIMEOUT_S` | `5.0` | per request; one retry on 429/5xx |
| `LHA_SYSTEM_ONE_PRICE_IN_PER_MTOK` | unset | unset: Jev's price for TypeSafe's host, $0 on loopback; any other endpoint must set it |
| `LHA_SYSTEM_ONE_PRIVATE_DATA_OK` | `false` | with `LHA_PRIVATE_DATA=true`, a remote endpoint is refused unless this is set |
| `LHA_SYSTEM_ONE_TRIAGE` | `true` | stall triage, when a backend is configured |
| `LHA_SYSTEM_ONE_TRIAGE_THRESHOLD` | `0.9` | the confidence an answer needs before it acts |
| `LHA_SYSTEM_ONE_TRIAGE_MIN_FAILURES` | `2` | failures in a row before triage is asked |
| `LHA_SYSTEM_ONE_RERANK_MIN` | `0.0` | `LHA_MEMORY_RERANK=system_one` drops passages whose relevance is below this |

A configuration that cannot be used safely stops the run before it starts. That covers a
non-https remote endpoint, a remote endpoint without a key, an endpoint with no price, and a
remote endpoint on a private-data run. A durable mission reports this as a configuration error
and does not retry.

## What leaves the machine

A remote endpoint receives the task description, the item's witnesses, the last two failure
reports (their final 3,000 characters) and, with reranking, up to 1,500 characters of each
recalled passage. All of it goes through the same secret redaction as untrusted web content
first. The connection follows the Voyage embedder's rules: https only, the host re-resolved and
checked for public addresses on every request, only vetted addresses dialled, no proxy from the
environment, and the key sent only to the endpoint host and scrubbed from errors.

The endpoint receives what the lead model's provider already receives, so a hosted System One
model does not widen the Rule of Two. `LHA_PRIVATE_DATA=true` still requires an explicit
`LHA_SYSTEM_ONE_PRIVATE_DATA_OK=true` for a remote endpoint, because it is a second party.

## Cost and budget

Every call goes through the mission's `CostMeter` under the ledger role `system_one`. It is
authorized against the budget before it is sent, priced at a conservative input-token estimate,
and recorded at the reported input tokens (output is free). `lha costs <mission>` lists these
calls. A call the budget refuses is skipped, like any other failed call. A triage question costs
about 2,000 input tokens: $0.00008 on Jev.

## The record

Every triage answer is committed with its cycle as a `system_one` event:

```json
{"kind": "system_one", "cycle_id": "c7", "payload": {
  "use": "stall_triage", "item_id": "03", "model": "jev-1.13.0", "answer": "scope",
  "confidence": 0.94, "probabilities": {"defect": 0.03, "scope": 0.96, "environment": 0.01},
  "threshold": 0.9, "action": "split", "error": ""}}
```

`model` is the versioned id that answered, so a changed threshold can be checked against the
answers it would have changed. A failed call records `action: "continue"` and the `error`. An
item blocked for an environment problem starts its `last_failure` with that reason, which the
deadlock gate question and the next attempt both show. Reranking answers are not committed; a
failed rerank is logged as `memory_rerank_failed`.

## Go

The Go implementation has the same settings, client, triage and reranker, checked against the
same cases (`spec/systemone/wire.json`): request bodies (including option order, which can change
a model's answer), strict answer parsing, the confidence formulas, the triage question, state and
action table, reranking, and endpoint prices. In Go, `LHA_MEMORY_RERANK=system_one` is the one
reranker that actually reranks (Go has no cross-encoder).

## Not built yet

These uses were considered and left for later, because each needs thresholds measured on real
LHA missions first:

- a pre-review screen of the diff for weakened or deleted tests (it would only force the full
  reviewer, never skip it);
- screening `fetch_url` and `web_search` results for instructions aimed at the agent (defence in
  depth; the Rule of Two stays the boundary);
- choosing the implementer's model tier by the item's difficulty;
- a second opinion on commands the classifier allows, sending only the command, never tool
  output.

LHA already produces labels for fitting those thresholds: a human's approve or reject, the
verifier's pass or fail, and the reviewer's verdicts. Exporting them as Kev fine-tuning data is
the planned next step. See [the roadmap](23-roadmap.md).

## Sources

- TypeSafe: [API reference](https://docs.typesafe.ai/api.md), [confidence](https://docs.typesafe.ai/confidence.md), [Jev 1.13 known weaknesses](https://docs.typesafe.ai/model-jaggedness/jev-1.13.md), [models and pricing](https://docs.typesafe.ai/models.md)
- Kev: [jaredpalmer/kev](https://github.com/jaredpalmer/kev) (API, models, published comparison with Jev)
- Independent tests: [JevBench](https://jevbench.xyz), [awesome-jev-robustness](https://github.com/stillmarcus24/awesome-jev-robustness), [Jev after eight days of independent tests](https://dev.to/aws-builders/jev-after-eight-days-of-independent-tests-level-with-mid-price-llms-behind-the-frontier-1c60)
