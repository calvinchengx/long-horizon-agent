#!/usr/bin/env bash
# Mutation audit of the Go safety code (docs/20-testing.md#mutation-audit): gremlins mutates the
# files below one change at a time and runs their package's tests; a mutant that LIVED (or timed
# out) is a behaviour no test pins. Exits 1 on a survivor that scripts/mutation_equivalents.txt
# does not list as a reviewed exception (a proven-equivalent mutant, or one a named test kills but
# gremlins cannot run).
#
# gremlins has no ignore comment, so the equivalents file names a mutant by what does not move
# when code is edited: "<MUTATOR> <path>:<column> <the mutated line's text, trimmed>", then "  # <why>" (two spaces).
set -euo pipefail
cd "$(dirname "$0")/.."
gremlins=${GREMLINS:-gremlins}
out=$(mktemp)
# A mutant that turns a loop into an allocating spin can eat a CI runner's memory before its test
# fails; cap each test binary's address space on Linux so it dies alone (macOS ignores -v).
if [ "$(uname)" = Linux ]; then
  ulimit -v "${MUTATION_ULIMIT_KB:-12582912}"
fi
# package -> exclude regexp (every file of the package, or below it, except its safety rules)
audit() {
  local pkg=$1 exclude=$2
  echo "== $pkg"
  "$gremlins" unleash "$pkg" -E "$exclude" --workers "${MUTATION_WORKERS:-4}" \
    --timeout-coefficient "${MUTATION_TIMEOUT_COEFFICIENT:-20}" -S ltc | tee /dev/stderr |
    sed "s|^|$pkg |" >> "$out"
}
audit ./internal/safety 'pystr/|idna|pinnedhttp|shlex|doc\.go'
audit ./internal/obs 'tracing/|events\.go|procmem'
audit ./internal/execution '/|dispatcher|docker|factory|open_|proc|sandbox'
audit ./internal/execution/egressproxy 'embed|events|proxy|urlsplit'

# "   LIVED CONDITIONALS_BOUNDARY at egress.go:83:11" -> "CONDITIONALS_BOUNDARY <path> <line text>"
known=$(sed -e '/^#/d' -e 's/[[:space:]]\{2,\}#.*$//' -e '/^[[:space:]]*$/d' scripts/mutation_equivalents.txt)
unexplained=0
while read -r pkg status mutator _at location; do
  file=${location%%:*}
  rest=${location#*:}
  line=${rest%%:*}
  col=${rest#*:}
  path="${pkg#./}/$file" # gremlins reports the file relative to the audited package
  text=$(sed -n "${line}p" "$path" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
  key="$mutator $path:$col $text"
  if ! grep -qxF "$key" <<< "$known"; then
    echo "unexplained $status: $key  ($path:$line)"
    unexplained=$((unexplained + 1))
  fi
done < <(grep -E '^\S+[[:space:]]+(LIVED|TIMED OUT|NOT COVERED) ' "$out" | sed 's/TIMED OUT/TIMED_OUT/; s/NOT COVERED/NOT_COVERED/')
if [ "$unexplained" -gt 0 ]; then
  echo "$unexplained surviving mutants: add a test that kills each, or list a reviewed exception in scripts/mutation_equivalents.txt" >&2
  exit 1
fi
