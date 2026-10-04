#!/usr/bin/env bash
# Build the UI and copy it into both servers (each embeds its own copy; CI checks they match the
# build): python/src/lha/serve/ui and go/internal/serve/ui.
set -euo pipefail
cd "$(dirname "$0")"
npx openapi-typescript ../spec/serve/openapi.json -o src/api.gen.ts > /dev/null
npx tsc --noEmit
npx vite build > /dev/null
for dest in ../python/src/lha/serve/ui ../go/internal/serve/ui; do
  rm -rf "$dest"
  mkdir -p "$dest"
  cp -R dist/. "$dest/"
done
echo "bundled: $(find dist -type f | wc -l | tr -d ' ') files, $(gzip -c dist/assets/*.js | wc -c | tr -d ' ') bytes of gzipped JS"
