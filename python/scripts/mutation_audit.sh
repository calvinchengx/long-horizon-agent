#!/usr/bin/env bash
# Mutation audit of the Python safety code (docs/20-testing.md#mutation-audit): mutmut mutates the
# modules in [tool.mutmut] of pyproject.toml one change at a time and runs their tests; a mutant
# that survives is a behaviour no test pins. Prints the summary and every survivor's diff.
set -euo pipefail
cd "$(dirname "$0")/.."
uv sync --quiet --group mutation
rm -rf mutants
uv run mutmut run --max-children "${MUTATION_WORKERS:-4}" > /dev/null
uv run mutmut results --all true | awk -F': ' '{print $2}' | sort | uniq -c
# survived, timeout and "no tests" all mean no test pinned the change
survivors=$(uv run mutmut results | awk -F': ' '$2 != "killed" {print $1}' | sed 's/^ *//')
for name in $survivors; do
  echo "== $name"
  uv run mutmut show "$name" | grep '^[-+] '
done
