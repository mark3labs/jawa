# Orchestrator foundation

This package supplies SQLite-backed projects and ordered Todo/Building/Done cards,
first-run admin setup and login, embedded authenticated persistent JetStream, and
credential rotation. `Command()` mounts it as `jawa orchestrator`.

## Current boundary

Building is a board state only. No execution attempts, durable dispatch, PR creation,
provider API adapters, CI/review polling, or server-driven SSE board updates exist yet.
Projects store GitHub/Forgejo repository metadata; cards optionally store an issue URL.
Done is currently a manually controlled board state, not a verified PR outcome.

## UI

`ui.templ` renders the UI using templ and shadcn-templ buttons. Datastar 1.0.4
provides declarative filtering. SortableJS is wrapped in the `jawa-board` custom
element; moves are authenticated, CSRF-protected HTTP POSTs followed by authoritative
server rendering. Invalid moves revert their DOM changes. Groups are project-scoped.
A native select supplies keyboard-accessible moves.

Rocket means **Datastar Pro Rocket**, not the unrelated npm package `@rocket/core`.
Its licensed distribution was not available in this workspace. This implementation
uses a standard custom-element fallback and does not claim to use Rocket. Supply
the licensed Rocket distribution before replacing the wrapper with its lifecycle API.

Frontend build: `cd frontend && npm ci && npm run build`. Assets and generated
`ui_templ.go` are checked in and embedded; production needs no Node runtime or CDN.
Datastar is vendored from the official v1.0.4 bundle at
https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js,
with its license in `frontend/DATASTAR-LICENSE.md`.

Regenerate templates from repository root:
`go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate -f internal/orchestrator/ui.templ`.

## Browser smoke test

Start a server with a **fresh**, private data directory on port 18080; run
`cd frontend && npm run test:smoke`. Set `CHROMIUM_PATH` to your Chromium executable
and `JAWA_SMOKE_URL` if changing the URL. The test creates local fixture projects/cards,
rotates local NATS credentials, exercises real pointer drag, and logs out/in. It does
not contact a Git provider or send agent work. Screenshot goes to the ignored
`.bonnie/orchestrator-smoke/board.png` directory (create that directory first).
