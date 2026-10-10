# Orchestrator integration

SQLite stores projects, ordered cards, sessions and durable execution attempts.
`jawa orchestrator` starts authenticated persistent JetStream and connects its
workflow using separate process-local internal credentials, unaffected by agent
credential rotation. Todo -> Building atomically records an attempt/outbox;
shared BONNIE tasks, events and results connect the board to agents.

See WORKFLOW.md for delivery semantics and PROVIDERS.md at repository root for
independent PR readiness policy. GET /events pushes authenticated Datastar SSE element patches; GET /snapshot
provides a finite refresh. Mutations return element/signal patches. Stable keyed
DOM roots preserve filtering, drafts, disclosures, and the page shell without reloads. No browser NATS connection is used.

## Views and UI build

Authenticated routes: `/board?project=<id>`, `/runs?state=<tab>&card=<id>`,
`/agents`, `/settings`. `/` redirects to `/board`. Board shows one project;
execution history, agent inspection, and configuration live in separate views.

Templates are separated into `layout.templ`, `board.templ`, `runs.templ`,
`agents.templ`, and `settings.templ`. Prebuilt shadcn-templ Card, Empty, Avatar,
Table, Breadcrumb, Kbd, Input, Badge, Alert, Button, Label, Textarea, Separator
and dialog primitives use compiled Tailwind/Nova styling with a dark Jawa palette.
Rocket light-DOM components encapsulate Sortable, native popover actions, shortcuts,
relative timestamps and clipboard behavior; templ retains ownership of markup.

Regenerate templates from repository root:
`go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate ./internal/orchestrator`.
Then `cd internal/orchestrator/frontend && npm ci && npm run build`.
Generated templ output and frontend assets are intentionally checked in and embedded;
production requires neither Node nor CDN access. The build locates shadcn components
using `go list -m` and copies the self-hosted Inter Latin variable font.

Datastar/Rocket is vendored from the official v1.0.4 bundle:
https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar-rocket.js.
It includes Datastar v1.0.4 and Rocket beta.2 (beta API). Datastar's license is in
frontend/DATASTAR-LICENSE.md, Inter's OFL license in frontend/INTER-LICENSE.

## Full offline browser smoke

Run `go run ./cmd/factory-smoke` after installing frontend dependencies. Set
CHROMIUM_PATH to your Chromium executable if needed. The command uses ephemeral
loopback ports, private temporary data, production orchestrator startup, and a
real BONNIE host/NATS channel/presence registry with an offline deterministic Kit
model. Browser-created cards actually publish to bonnie.tasks, enter running,
produce results, and remain blocked without provider credentials. The smoke checks
live presence/history, rejection of manual Done, pointer drag, keyboard moves,
filtering, persistence, credentials/login, and agent shutdown/unregistration.
It makes no paid model calls, pushes no branches, and creates no real PRs.

Provider fixtures and TestWorkflowProviderVerifierToDone cover independent readiness:
real NATS outcome -> pending CI blocker -> green fixture CI -> verified Done.
Live model-driven repository edits and live GitHub/Forgejo PR/CI/review cycles are
not validated by these offline tests.

## Remaining limitations

Single orchestrator per database/NATS scope. Shared queue rather than capacity-aware
assignment. Presence is advisory, not an execution lease. No cancellation or automatic
agent failover. Retry after a terminal agent outcome; legacy Building cards without an attempt
can Start work. Reset/Delete are guarded against active executions; deletion
requires confirmation and Reset preserves attempt history. Time/cost bounds are prompt
guidance rather than scheduler enforcement. Done is PR-ready, not merged, and later
review changes do not automatically reopen the card. Remote HTTP/NATS require external
TLS/network controls; do not share the orchestrator's data or credentials with agents.
