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

`GET /projects/issues?project_id=<id>` requires an authenticated session and
returns JSON `{"issues":[{"number":2,"title":"Example","url":"https://github.com/owner/repo/issues/2"}]}`.
Only open issues (not pull requests) from the configured project are listed;
empty lists return `{"issues":[]}`. Errors use 401 for missing authentication,
404 for an unknown project, 405 for non-GET methods, and 502 for provider loading
failures. Provider credentials and response bodies are never returned to the
browser. The New card Rocket selector loads on opening, project changes and
explicit refresh; loading failures do not prevent creating an unlinked card.
Run `cd internal/orchestrator/frontend && CHROMIUM_PATH=/path/to/chromium npm run test:issues`
for isolated selector browser tests (including stale-response races).

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

## Mobile and PWA checks

The public `/sw.js` endpoint serves the embedded worker with `no-cache`, before
login. The manifest, icons and generic offline page live under `/assets/`.
`frontend/src/sw.js` explicitly allowlists static assets; navigation uses the
network, falling back only on network failure. Authenticated HTML, API calls,
SSE and mutations are never cached. `npm run build` hashes the worker source
and all precached assets to version the cache. It does not force activation
while an old app window is open. Icon PNGs derive from `frontend/icon.svg`:
`magick -background '#130f0b' frontend/icon.svg -resize 192x192 assets/icon-192.png`
(from this directory); use 180/512 for the other sizes. The padded 512 image
also supplies the maskable icon.

For browser checks, build the binary after regenerating embedded assets, then
start a disposable server from the repository root:

```sh
go build -o ./bin/jawa .
./bin/jawa orchestrator --data-dir /tmp/jawa-mobile-test --listen 127.0.0.1:18081 --nats-listen 127.0.0.1:14223
# In another terminal:
cd internal/orchestrator/frontend
JAWA_SMOKE_URL=http://127.0.0.1:18081 JAWA_RESPONSIVE_SETUP=1 npm run test:responsive
JAWA_SMOKE_URL=http://127.0.0.1:18081 npm run test:pwa
```

Use a fresh data directory: responsive setup creates an administrator, configures
NATS and creates a project/card/run (no agent or external provider needed).
For subsequent runs use `JAWA_RESPONSIVE_STORAGE_STATE` with a Playwright state
file saved via `JAWA_RESPONSIVE_SAVE_STATE`, plus `JAWA_RESPONSIVE_BOARD_PATH`
if the board URL must select a project. Checks cover 320/390/768/1024/1440 widths,
long names/titles, touch targets, run fields, menus, dialogs and overflow.
The PWA check uses its own unauthenticated browser context and verifies actual
worker registration, public-only cache contents, offline navigation and recovery.
Set `CHROMIUM_PATH` for both scripts if needed. Physical iOS/Android installation
and device-specific browser chrome still require manual testing.

### Authentication persistence

New setup/login sessions last exactly 30 × 24 hours, without sliding renewal.
Existing rows retain their expiry; log in again after upgrading. Logout revokes
access immediately. Retain the same origin, profile and server data directory;
storage deletion/eviction may require login. A missing CSRF cookie is regenerated
without renewing authentication.

Against a **separate fresh disposable server/data directory**, run:

```sh
JAWA_SMOKE_URL=http://127.0.0.1:18081 npm run test:auth-persistence
```

This creates test-only administrator and NATS credentials through the forms.
It closes/relaunches Chromium with a temporary persistent profile (no exported
storageState), verifies worker control, 30-day cookies, refresh, protected reads,
CSRF-protected logout and public-only caches. Set `CHROMIUM_PATH` if needed.
This is browser-profile/service-worker coverage, not native installed-PWA proof.
The existing `test:pwa` remains an independent unauthenticated check.

Manual installed-PWA acceptance (record unavailable platforms as **untested**):

1. Record OS/browser versions and installation origin. On available iOS standalone
   Safari and Android installed Chrome, log in **inside the installed app**;
   browser/app cookie-store separation may require this initial login.
2. Close the app completely and reopen from its icon at the same origin. Require
   authenticated workspace access and a CSRF-protected action.
3. Repeat with refresh, device/app restart and server restart within the 30-day
   window, retaining the profile and server data directory.
4. Log out, close and reopen; require login. Do not persist passwords or bearer
   tokens in localStorage/IndexedDB as a workaround.

If immediate authentication loss persists, acceptance is not satisfied. Collect
only redacted metadata: cookie presence before/after relaunch, expiry/security
attributes (no values), whether the launch request sends a session cookie,
response status/redirect location, and whether the same database is retained.
Distinguish cookie loss, origin mismatch, server row loss and actual expiry before
proposing another change. Do not weaken SameSite without reproduction and review.
Physical iOS/Android lifecycle validation is untested by this automated check.

## Full offline browser smoke

Run `go run ./cmd/factory-smoke` after installing frontend dependencies. Set
CHROMIUM_PATH to your Chromium executable if needed. The command uses ephemeral
loopback ports, private temporary data, production orchestrator startup, and a
real BONNIE host/NATS channel/presence registry with an offline deterministic Kit
model. Browser-created cards actually publish to bonnie.tasks, enter running,
call actual typed completeTask tools, receive commit-after-transaction report ACKs, and remain blocked without provider credentials. The smoke checks
live presence/history, rejection of manual Done, pointer drag, keyboard moves,
filtering, persistence, credentials/login, and agent shutdown/unregistration.
It makes no paid model calls, pushes no branches, and creates no real PRs.

Provider fixtures and TestWorkflowProviderVerifierToDone cover independent readiness:
typed report DB commit -> pending CI blocker -> green fixture CI -> verified Done.
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

## Run detail

`GET /runs/{attemptID}?tab=chat|trace` uses the shared authenticated layout,
Runs navigation and its own stream `GET /runs/{attemptID}/events?tab=...` (never
the generic Board/Runs SSE). Unknown/unpinned attempts are 404. Session checks,
no-store, setup guards and CSRF apply; unauthenticated detail streams are 401.
`POST /runs/{attemptID}/chat` accepts bounded `text`; `POST .../stop` requires
the displayed `turn_id`. Browser fields never select agent subject/run identity.
Enhanced actions disable retry and return finite notices; native actions redirect.

`run-detail-content` morphs status and history, `run-detail-controls` morphs
eligibility and exact-turn Stop. Tabs, Trace filter and composer/draft stay
outside both roots. Only confirmed send success clears `runDraft`; uncertain
mutations preserve it. Assistant Markdown uses the existing sanitizer; other
content is escaped and no remote media is loaded. BONNIE display adaptation
attribution is retained in BONNIE-LICENSE.

Factory smoke includes two simultaneous viewers, follow-up, committed response,
draft retention, Trace filter, 320/390/768/1024/1440 overflow checks and real
Chromium desktop/mobile screenshots under ignored node_modules/.factory-smoke.
See README Run detail for reduced trace fidelity, bounds and the explicitly
accepted trusted-worker JetStream delivery-proxy limitation.
