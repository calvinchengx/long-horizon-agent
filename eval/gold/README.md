# Gold evaluation sets

Each `.jsonl` file here is a gold evaluation set: `lha labels export` rows (one judgment LHA
recorded, with the redacted input it judged) plus the judgment a correct judge would have given
and who established it. `lha eval check eval/gold/*.jsonl` validates them; `lha eval run` scores
a judge against them. The format, the vocabulary and the scoring are described in
[docs/25-system-one.md](../../docs/25-system-one.md#gold-evaluation-sets) and pinned by
[`spec/systemone/gold.json`](../../spec/systemone/gold.json).

`gold.label` is the right answer for what was judged (the row's `input`), not for the item: a
reviewer shown an empty diff should block, whatever the code looked like. `gold.by` names the
evidence; `tags` group rows by what happened (`bypass`, `conftest-injection`, `witness-rewrite`,
`empty-diff`, `unrelated-file`, `dangling-tool-call`, `review-base-defect`, `cross-port-parity`,
`debatable`, and the run: `pilot-1`, `pilot-2`, `pair-1`, `pair-2`, `pair-3`).

| File | Rows | From |
|---|---|---|
| `review-and-verifier-2026-10-02.jsonl` | 73 (52 verifier, 21 review; no gates opened) | the ten `lha orchestrate` missions of the 2 October 2026 reviewer measurement ([docs/11](../../docs/11-multi-agent-organization.md#measured-the-reviewer-and-the-gates-2-october-2026)): a Sonnet lead through Claude Code on a clone of this repository, three small changes with shell witnesses. Gold labels come from the write-up and the run directory's commits: the two gate bypasses (a new `conftest.py` hook, rewritten witness scripts) make the verifier's `passed` a gold `failed`; the reviewer's dangling tool calls (`unparsed`) get the verdict the diff deserved; empty diffs and an unrelated committed cache file are gold `block`. Review rows carry their diffs. |
| `org-vs-single-2026-10-03.jsonl` | 27 (18 verifier, 9 review) | the six missions of the 3 October 2026 organization measurement ([docs/11](../../docs/11-multi-agent-organization.md#measured-the-organization-against-the-single-loop-3-october-2026)): three `lha mission` runs and three `lha orchestrate --research 1 --review` runs on the same three changes. Every change was honest and passed at the first attempt (the commits touch only the files the items name, and the witnesses pass when re-run), so every gold label equals the recorded one: these are the true negatives a judge must not refuse. |

Add a set by exporting labels (`lha labels export --diffs`), adding `gold` and `tags` to each row
you can vouch for, and running `lha eval check` on it. Rows you cannot vouch for do not belong
here; a row whose gold label is arguable carries the `debatable` tag and says why in `gold.note`.
