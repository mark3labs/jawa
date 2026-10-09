# Orchestrator integration

SQLite stores projects, ordered cards, sessions and durable execution attempts.
`jawa orchestrator` starts authenticated persistent JetStream and connects its
workflow using separate process-local internal credentials, unaffected by worker
credential rotation. Todo -> Building atomically records an attempt/outbox;
shared BONNIE tasks, events and results connect the board to workers.

See WORKFLOW.md for delivery semantics and PROVIDERS.md at repository root for
independent PR readiness policy. GET /events pushes authenticated Datastar SSE element patches; GET /snapshot
provides a finite refresh. Mutations return element/signal patches. Stable keyed
DOM roots preserve filtering, drafts, disclosures, and the page shell without reloads. No browser NATS connection is used.

## UI build

`ui.templ` uses templ and prebuilt shadcn-templ Card, Badge, Input, Textarea,
Label, Alert, Separator, Button and dialog primitives with compiled Tailwind/Nova CSS. Datastar 1.0.4 supplies declarative
filtering. SortableJS lives in `jawa-board`, a project-scoped standard custom element
with lifecycle cleanup, rollback on rejected moves, and keyboard-accessible selects.
Datastar Pro Rocket is licensed and unavailable in this workspace; this wrapper is
an explicit fallback, not a claim of Rocket integration.

From repository root regenerate templates:
`go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate -f internal/orchestrator/ui.templ`.
Then `cd internal/orchestrator/frontend && npm ci && npm run build`.
Generated `ui_templ.go` and frontend assets are intentionally checked in and embedded;
production requires neither Node nor CDN access. Datastar is vendored from
https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js
with its license in frontend/DATASTAR-LICENSE.md.

## Full offline browser smoke

Run `go run ./cmd/factory-smoke` after installing frontend dependencies. Set
CHROMIUM_PATH to your Chromium executable if needed. The command uses ephemeral
loopback ports, private temporary data, production orchestrator startup, and a
real BONNIE host/NATS channel/presence registry with an offline deterministic Kit
model. Browser-created cards actually publish to bonnie.tasks, enter running,
produce results, and remain blocked without provider credentials. The smoke checks
live presence/history, rejection of manual Done, pointer drag, keyboard moves,
filtering, persistence, credentials/login, and worker shutdown/unregistration.
It makes no paid model calls, pushes no branches, and creates no real PRs.

Provider fixtures and TestWorkflowProviderVerifierToDone cover independent readiness:
real NATS outcome -> pending CI blocker -> green fixture CI -> verified Done.
Live model-driven repository edits and live GitHub/Forgejo PR/CI/review cycles are
not validated by these offline tests.

## Remaining limitations

Single orchestrator per database/NATS scope. Shared queue rather than capacity-aware
assignment. Presence is advisory, not an execution lease. No cancellation or automatic
worker failover. Retry after a terminal worker outcome; legacy Building cards without an attempt
can Start work. Reset/Delete are guarded against active executions; deletion
requires confirmation and Reset preserves attempt history. Time/cost bounds are prompt
guidance rather than scheduler enforcement. Done is PR-ready, not merged, and later
review changes do not automatically reopen the card. Remote HTTP/NATS require external
TLS/network controls; do not share the orchestrator's data or credentials with workers.
