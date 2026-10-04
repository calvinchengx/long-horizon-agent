# The mission UI

A static bundle every implementation's `lha serve` embeds and serves at the start-up URL it
prints ([docs/27-mission-ui.md](../docs/27-mission-ui.md)). It speaks only the UI API
([`spec/serve/openapi.json`](../spec/serve/openapi.json)); its types are generated from that
contract, so it works the same against the Python and the Go server.

Vite, TypeScript and Preact (`preact-iso` routing), the browser's `EventSource` for live updates,
plain CSS with light and dark themes. About 15 KB of gzipped JavaScript.

```bash
npm ci
./bundle.sh        # regenerate src/api.gen.ts, typecheck, build, copy into both servers
npm run dev        # against a running `lha serve` on 127.0.0.1:8765 (LHA_SERVE_URL)
```

`bundle.sh` writes the build to `python/src/lha/serve/ui/` and `go/internal/serve/ui/`, which are
committed (the Go binary embeds its copy). CI rebuilds from source and fails when either copy, or
the generated types, differ from what is committed.

Writes need the `X-LHA-Token` header: the server puts the token in the page it serves, and only to
a browser that already holds the token cookie (set by opening the start-up URL). The page's
Content-Security-Policy allows this origin only, and it sends no `Referer`.
