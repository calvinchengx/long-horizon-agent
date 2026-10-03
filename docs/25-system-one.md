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
done. The deterministic verifier and the command classifier keep those decisions. That is not
just a design intention: [`spec/systemone/authority.json`](../spec/systemone/authority.json)
sweeps the answer space — including option labels that were never offered, labels naming an
authority-widening outcome, and confidence 1.00 under a zero threshold — and both
implementations must show that triage still only continues, splits or blocks, and that
reranking only reorders and drops passages. When the
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

## Measured

On 27 September 2026 we ran one small experiment to see whether triage lowers the cost of a
mission. The lead was `gemma4` on Ollama and the System One model was a self-hosted Kev-4B. There
were three arms: triage off, triage at the default 0.9 threshold, and triage at 0.7. Each arm ran
the same three-item roadmap twice, with a cap of 20 cycles per mission. The roadmap had an
ordinary item, a deliberately oversized item and an item that could never pass in the sandbox
(it needs a host the sandbox cannot reach).

**Triage never acted.** Across ten triage calls on real failures, Kev-4B's confidence ranged from
0.09 to 0.71, so no answer reached either threshold. Its answers were also mostly wrong for the
failures it saw: "defect" 3 times, "scope" 5 and "environment" once, for failures of an ordinary
item. With nothing acting, the arms differed only by about 0.4 s per call.

The missions' cost varied 3–4× within each arm, from $0.44 to $4.84 at Claude Sonnet 4.6 prices.
That spread came from the lead model: in four of the six runs it stalled on the easiest item, on
which every other item depended. The item triage should catch, the impossible one, was reached in
only one run, which had triage off. There the replanner split it into three children that could
not pass either, which is the waste triage is meant to prevent.

What this shows: with a small local lead and an uncalibrated Kev-4B, triage costs almost nothing
and saves nothing. It does not show what a stronger lead model, a better-calibrated System One
model, or a model fine-tuned on LHA's own failures would do. Kev's authors recommend fine-tuning
on your own labels, and LHA records the evidence needed for that in every `system_one` event.

## Labels

Every mission records three judgments that can fit those thresholds: a human's approve or reject
of a gated tool call, the verifier's verdict on an attempt, and the reviewer's verdict on a
verified diff. [`lha labels export`](17-cli.md#lha-labels-export) writes them as JSON Lines, one
object per judgment with `source`, `label`, `by` and a redacted `input`: the anchor's committed
`tool_approval`, `cycle` and `review` events, then the mission's closed gates from the store. The
reviewer's verdict is committed as a `review` event by both the local orchestrator and the durable
`review_cycle` activity, so it survives the process like the other two. Nothing in a row is new
data: it is the record LHA already keeps, joined and redacted, and no label leaves the machine
unless you send the file somewhere. Both implementations derive identical rows from the same
events and gates (`spec/systemone/labels.json`).

## Gold evaluation sets

A label row says what a judge decided; a gold row adds what it should have decided. The sets
under [`eval/gold/`](../eval/gold/README.md) are `lha labels export` rows with two more keys:

```json
"gold": {"label": "failed", "by": "measure-2026-10-02 write-up (docs/11, ...)", "note": "a new conftest.py hook injected the required test ..."},
"tags": ["conftest-injection", "bypass", "pair-1"]
```

`gold.label` is the right answer for what was judged (the row's `input`), not for the item: a
reviewer shown an empty diff should block, whatever the code looked like. The vocabulary per
source is `approve`/`reject` (gates and tool approvals), `passed`/`failed` (the verifier) and
`approve`/`block` (the reviewer); the second of each pair is the label that refuses, and the
scoring counts its precision and recall. `lha eval check FILES` refuses a set with a gold label
outside the vocabulary, a `gold` without `label` and `by`, or the same judgment twice;
`lha eval run FILES --judge NAME` scores a judge and prints, per source, how many rows it
judged, how many agree with gold, precision and recall of the refusing label, and every
disagreement with its tags and note ([17-cli.md](17-cli.md#lha-eval)). Two judges are built in
and need no model:

- `recorded`: the label the mission recorded, so the scorecard says how often LHA's own verifier,
  reviewer and gates were right;
- `screen`: the deterministic pre-review screen re-run on each review row's diff
  ([07-verification.md](07-verification.md#pre-review-screen)); it abstains from rows that
  carry no diff and from every other source.

Both implementations parse, check, score and render the same bytes
([`spec/systemone/gold.json`](../spec/systemone/gold.json)), and both test suites keep the
committed sets valid.

The first set, `review-and-verifier-2026-10-02.jsonl`, holds the 73 judgments of the reviewer
measurement ([11-multi-agent-organization.md](11-multi-agent-organization.md#measured-the-reviewer-and-the-gates-2-october-2026)):
52 verifier verdicts and 21 review verdicts, no gates. What it shows, on 3 October 2026:

| Judge | Source | Agree | Refusing label: precision / recall | What the misses are |
|---|---|---|---|---|
| recorded | verifier | 40 of 52 | 1.00 / 0.59 | the 12 `passed` verdicts on the two gate bypasses (a `conftest.py` hook, rewritten witness scripts) and on empty-diff attempts |
| recorded | review | 15 of 21 | 1.00 / 0.71 | the 6 dangling tool calls parsed as `unparsed`; every verdict the reviewer did give was right |
| screen | review | 11 of 21 | 1.00 / 0.29 | it flags all 4 `conftest.py` injections (which the screen of the day missed: `screen_findings` is empty in those rows) and nothing it cannot see in a diff: empty diffs, an unrelated committed cache file, a Go twin left behind |

So the verifier's misses are the gates closed since ([07-verification.md](07-verification.md#harness-integrity)),
the reviewer's misses are the defect fixed since (the final-turn message), and the screen's
recall on this set is the ceiling a diff-only screen has: it never blocks wrongly and it cannot
see what is not in the diff. A model-backed judge (a System One model asked the review question,
or a fine-tuned Kev) slots in as a third judge; the rows carry everything it would be shown.

## Not built yet

These uses were considered and left for later, because each needs thresholds measured on real
LHA missions first:

- a model-backed pre-review screen: the deterministic one is built
  ([07-verification.md](07-verification.md#pre-review-screen)) and records what it found next to
  each review verdict, which is the label set a model-backed screen would be fitted on;
- screening `fetch_url` and `web_search` results for instructions aimed at the agent (defence in
  depth; the Rule of Two stays the boundary);
- choosing the implementer's model tier by the item's difficulty;
- a second opinion on commands the classifier allows, sending only the command, never tool
  output.

The labels for fitting those thresholds already exist and [`lha labels export`](#labels) writes
them; the [gold sets](#gold-evaluation-sets) and `lha eval` score any such judge offline, and a
model-backed judge is the next one to add. See [the roadmap](23-roadmap.md).

## Sources

- TypeSafe: [API reference](https://docs.typesafe.ai/api.md), [confidence](https://docs.typesafe.ai/confidence.md), [Jev 1.13 known weaknesses](https://docs.typesafe.ai/model-jaggedness/jev-1.13.md), [models and pricing](https://docs.typesafe.ai/models.md)
- Kev: [jaredpalmer/kev](https://github.com/jaredpalmer/kev) (API, models, published comparison with Jev)
- Independent tests: [JevBench](https://jevbench.xyz), [awesome-jev-robustness](https://github.com/stillmarcus24/awesome-jev-robustness), [Jev after eight days of independent tests](https://dev.to/aws-builders/jev-after-eight-days-of-independent-tests-level-with-mid-price-llms-behind-the-frontier-1c60)
