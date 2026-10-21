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

## Settings: MCP access

Settings includes an MCP access panel (`mcp-access`) for creating and revoking
full-access personal keys. It shows prefixes, creation/last-used times and
Active/Revoked state, never hashes or recoverable secrets. Listener address
overrides apply after restart; empty fields retain overrides, Reset clears them.

The one-time key creation response is the single exception to the rule against
rendering credentials. Both its Datastar dialog and native fallback are no-store.
Secrets must never enter logs, flash, signals, cookies or browser storage. The
`apikeys` dialog name has no hyphen so pending signals and indicators agree;
the Go and Datastar browser fixtures cover its open/close, operation IDs and
inline errors. See MCP.md for the remote endpoint contract.

## Views and UI build

Authenticated routes: `/board?project=<id>`, `/runs?state=<tab>&card=<id>`,
`/agents`, `/settings`. `/` redirects to `/board`. Board shows one project;
execution history, agent inspection, and configuration live in separate views.

`GET /dialogs/card/issues?project_id=<id>` renders open issue choices over finite
SSE using replace mode, a fresh submission token and local `_issues` signals for
explicit Autofill. Provider errors are sanitized; issue bodies are not logged.
Issue requests are bounded to 20 seconds. No JSON choices endpoint remains.

The templ UI uses pinned shadcn-templ components for controls, Dialog and
DropdownMenu behavior. Every dialog opening is rendered by `GET /dialogs/{name}`
and carries a unique operation-keyed frame outside live roots. Submit success
removes only that frame, after every other response frame. See `SSE_UI.md`.

Regenerate templates from repository root:
`for file in internal/orchestrator/*.templ; do go run github.com/a-h/templ/cmd/templ@v0.3.1070 generate -f "$file"; done`.
Use repository-relative `-f` paths to preserve diagnostic filenames consistently
with CI; `-path internal/orchestrator` writes different diagnostic filenames.
Then `cd internal/orchestrator/frontend && npm ci && npm run build`.
Generated templ output and frontend assets are intentionally checked in and embedded;
production requires neither Node nor CDN access. The build locates shadcn components
using `go list -m`, bundles the pinned Select, Base UI/Floating UI, checkbox/switch runtimes and lifecycle with the
application (no CDN), and copies the self-hosted Inter Latin variable font.

Datastar is vendored from the official v1.0.4 bundle:
https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.4/bundles/datastar.js. Datastar's license is in
frontend/DATASTAR-LICENSE.md, Inter's OFL license in frontend/INTER-LICENSE.

All browser scripts share `frontend/browser.mjs`, including the persistent-profile
authentication check. With `CHROMIUM_PATH` unset, they use Playwright's bundled
Chromium: run `npx playwright install chromium` in `frontend` after `npm ci`.
Set `CHROMIUM_PATH` to use an existing executable. `npm run test:browser` checks
the shared launch options without starting a browser.

## First-paint and card fixture checks

Export deterministic **real Board and Archive pages** (the Archive export uses
`.archive` as a suffix) from the repository root:

```sh
JAWA_CARD_VISUAL_HTML=/tmp/jawa-first-paint.html go test ./internal/orchestrator -run '^TestCardVisualFixture$' -count=1
cd internal/orchestrator/frontend
JAWA_CARD_VISUAL_HTML=/tmp/jawa-first-paint.html CHROMIUM_PATH=/path/to/chromium JAWA_FIRST_PAINT_SCREENSHOT_DIR=/tmp/jawa-first-paint-images npm run test:first-paint
JAWA_CARD_VISUAL_HTML=/tmp/jawa-first-paint.html CHROMIUM_PATH=/path/to/chromium npm run test:cards
```

The first-paint check serves only fixtures and embedded assets, blocks service
workers, and gates CSS and JavaScript independently with bounded cleanup. It
covers cold loads, reloads, native Board→Archive links and back at 390/1440px.
DOM geometry sampled while CSS is held is **not evidence of paint**: the normal
stylesheet is render-blocking. Failed CSS with JavaScript held supplies an
actually paintable fallback; paint timing, colors and bounded icon sizes are
checked before releasing JavaScript. Successful CSS with delayed/disabled JS
must remain visible and preserve contextual icon sizes. Optional Chromium PNGs
capture fallback and styled loading intervals; these are local test artifacts,
not committed assets. Exact reporter-flash reproduction and physical-device
checks must be recorded separately, not inferred from these regressions.

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
`POST /runs/{attemptID}/chat` accepts UTF-8 `text` of 1–64000 bytes, a UUID `message_id` and displayed `interaction` token; `POST .../stop` requires
the displayed `turn_id`. Browser fields never select agent subject/run identity.
Enhanced actions disable retry and return finite notices; native actions redirect.

`run-detail-content` holds status and history, `run-detail-controls` holds
eligibility and exact-turn Stop. The stream sends both in full on its first tick,
then only roots that changed: `run-detail-status`, `run-detail-controls`, and
`run-live`/`run-pending` (Chat) or `run-trace` (Trace). New committed chat entries
are appended to `#run-messages`; a changed entry (a call whose result arrived) is
morphed by its key; any other transcript change replaces `#run-messages`.
Tabs, Trace filter/source toggles and composer/draft stay outside all roots. Only durable message acceptance clears `runDraft` when it still equals the submitted
text; newer drafts and uncertain mutations survive. Externally associated Send
controls stay disabled while pending without overriding server policy. Assistant Markdown uses the existing sanitizer; other
content is escaped and no remote media is loaded. BONNIE display adaptation
attribution is retained in BONNIE-LICENSE.

Factory smoke includes two simultaneous viewers, follow-up, committed response,
draft retention, Trace filter, 320/390/768/1024/1440 overflow checks and real
Chromium desktop/mobile screenshots under ignored node_modules/.factory-smoke.
See README Run detail for reduced trace fidelity, bounds and the explicitly
accepted trusted-worker JetStream delivery-proxy limitation.

Chat adapts Bonnie’s ordered messages and compact role markers within Jawa’s
theme. A tool call and its result, paired by tool call ID, share one collapsed
row: the summary names what the call acts on (command, path, pattern) and the
result's status and short output. Error results and nonzero exit codes mark the
row and show a destructive badge; stderr marks it as a warning. Unified diffs in
edit output are colored per line. Nested sections start open within closed rows
and have copy buttons that copy from their own code element; assistant code
blocks get the same button. Raw message/result JSON is in Trace only; unsupported
parts show a pointer to Trace. User messages after the first start an exchange,
labelled with the acceptance time when a delivery receipt places the message.
All payloads are escaped (JSON and diff spans use fixed classes), and Markdown keeps
the stricter no-remote-media sanitizer; copy buttons are added after sanitizing.

While a turn runs, `run-live` shows the uncommitted tail of the current turn
from live events: streamed text (prefixed with an ellipsis when the bounded live
buffer dropped earlier events), tool executions with running/done/error state,
and a working indicator. The next committed snapshot replaces it. Delivery
records not yet in the transcript, failed or uncertain render at the transcript
end with a status badge (Queued, Sending, Delivered, Failed, Uncertain), and
uncertain ones carry their own reconcile form. Internal classifications are not shown.

The chat view is a viewport-tall column: the transcript fills the space left by
the page chrome (at least 16rem) and the composer sits directly below it. The
growing composer uses Ctrl+Enter and native validation, ignores IME submission
and leaves plain Enter as a newline. The focusable transcript follows only at
bottom, preserves scrolled-up readers and offers a scroll-to-end button.
Component runtimes own lifecycle cleanup.

Trace has a Conversation snapshot table (Role/Summary) and one Events table that
merges durable and live events by sequence (durable first at equal sequence,
live arrival order kept). Event columns are Source, Sequence, Time (UTC), Kind,
State and Summary, omitting optional columns with no values. No roles are
inferred. Only supplied nonempty turn IDs get separator rows, which stay visible
while any of their entries match. Snapshot/Durable/Live toggles hide sources.
No local workflow deliveries or outcomes are rendered. Empty sources, missing
metadata and no-match filters have no explanatory placeholders or banners.
Every disclosure has a deterministic content/occurrence display ID and preserves
`open` during morphs. Snapshot keys are not journal IDs. Filtering reads each
entry's own text, including collapsed raw content, so no searchable copy of the
payloads is rendered. Draft/filter/tab elements stay outside the patch roots.

Export real deterministic Chat/Trace and stale offline fixtures, then check them:

```sh
JAWA_RUN_DETAIL_HTML=/tmp/run-detail go test ./internal/orchestrator -run '^TestRunDetailVisualFixture$' -count=1
cd internal/orchestrator/frontend
JAWA_RUN_DETAIL_HTML=/tmp/run-detail CHROMIUM_PATH=/path/to/chromium npm run test:run-detail
```

Optional `JAWA_RUN_DETAIL_SCREENSHOTS=/tmp/run-detail-images` captures Chromium
390/1440px screenshots for both tabs/offline. Tests cover five viewport widths,
entry filtering, collapsed defaults, touch targets, no-JS links, keyboard
activation and actual Datastar stream patches preserving disclosure/draft/focus.
These fixtures are not evidence of external provider behavior or mobile devices.

## New card issue submission

`POST /cards` accepts optional `create_issue=yes`, `issue_submission` (opaque
32-byte hex token), and recovery fields `issue_recovery=link|unlinked|retry` and
`recovery_url`. Missing create_issue remains off for legacy clients. UI default
is on. Creation and issue_url are mutually exclusive. Durable reservations prevent
concurrent/replayed POSTs; interrupted reservations become uncertain at startup.
Completed evidence survives card deletion. Recovery is explicit and uses the
original submitted draft, validating same-repository canonical issue links.
Enhanced errors patch recovery/notice only; native errors retain escaped fields.
Only confirmed success closes/resets. The form disables Datastar retries and
locks submit buttons while pending; named inputs remain enabled for FormData. Read-only tokens work
when creation is off; Issues write permission is required when on. Check the
repository after uncertainty: remote/local writes cannot be atomic and issues
are never automatically retried. Offline browser fixtures must uncheck creation.

Real generated New card / recovery browser fixtures (no remote writes):

```sh
JAWA_ISSUE_FIXTURE="$PWD/internal/orchestrator/frontend/node_modules/.issue-fixture.html" go test ./internal/orchestrator -run '^TestIssueRecoveryFixture$' -count=1
cd internal/orchestrator/frontend
JAWA_ISSUE_FIXTURE="$PWD/node_modules/.issue-fixture.html" JAWA_ISSUE_SCREENSHOTS="$PWD/node_modules/.issue-screenshots" npm run test:issue-recovery
```

This checks exact enhanced form fields, retained draft/token, uncertainty controls,
keyboard/touch interaction, mutually exclusive controls and overflow at
1440/390/320px, with optional real Chromium screenshots. No live issue is created.

## Application component vocabulary

Visible controls use the pinned shadcn-templ Button (including action links),
Input, Textarea, Label, Select/Trigger/Value/Content/Item, Checkbox and Switch. Status/count
pills use Badge, warnings use Alert, menu dividers use Separator and card
surfaces use Card. Native forms, disclosures, ordinary text
links and structural layout remain native. Hidden transport fields (including
move-form fields) are intentionally not components.

Checkbox/Switch attributes target the focusable span, **not** its adjacent
native input. Use deterministic input IDs and uncontrolled DefaultChecked for
browser-owned fields. Signal-bound acknowledgements use the shared wrapper
bridge: native change writes the signal; a Datastar effect synchronizes required,
native checked, ARIA and indicator state through the pinned checkbox API. Do not
put data-bind on the span. Settings switches must precede hidden false fallbacks.
The bundled lifecycle, checkbox and switch runtimes handle SSE additions; native
no-script forms expose the adjacent input as a usable fallback.

Export real Settings/dialog markup with `JAWA_COMPONENT_FIXTURE=/path/components.html
go test ./internal/orchestrator -run 'Test(ComponentBrowserFixture|PlanningComponentFixture|AgentsComponentFixture)$' -count=1`,
then run `JAWA_COMPONENT_FIXTURE=/path/components.html npm run test:components`
in frontend. Native recovery is exported alongside the issue fixture as `.native`.

## Explicit agent assignment

Card menus offer **Plan with agent** and **Build with agent** in Todo,
**Build with agent** in Planning, and **Fix with agent** in Ready for review.
Todo builds start immediately without plan approval. Only generated plans need
review: Planning shows the rendered revision and one acknowledgement, then
**Approve & build with agent** freezes that exact revision in the existing
approval transaction. A stale revision is rejected; refresh the page to review
the new plan. Until a plan is ready, the dialog links to the planning view.
Fix requires a verified PR and a nonempty repair message, preserving its branch.
Existing shared-queue actions, drag/drop and ordinary Approve & build remain shared.

The selector lists connected agent IDs with advisory provider/model metadata and
availability. Busy input-capable agents are selectable; this is not a capacity
reservation or execution lease. Planning and build selections are independent.
The desired target is durable per task, separate from the observed run owner.
Publishing, restart recovery and fresh Retry retain it even after presence expires.
Queued targeted work waits for the selected stable ID within broker retention;
it never falls back to shared delivery or another agent. Replies, guidance,
cancellation and Resume still address the observed execution, not a new task.
Assignment fences admission but is not isolation against hostile trusted agents.

`GET /dialogs/assign/agents` is authenticated, setup-guarded, GET-only and no-store.
Discovery is bounded to two seconds; the rendered choices contain only agent ID, provider,
model, availability and selectable capability. Targeted mutations revalidate
this projection before their atomic lane/task transaction. Shared actions do not
require presence. No browser-supplied subject, PR URL, branch or run selects work.
The assignment dialog stays outside SSE patch roots. It loads on open/Refresh,
aborts and invalidates stale reads, and retains drafts on failed submissions.
Successful `/cards/assign` responses remove only their operation-keyed frame.
Run `npm run test:agents` in the frontend directory for selector race regressions.

Back up the database before migration 00019 and coordinate backend deployment.
Existing streams must include `bonnie.tasks.agent.*`; existing agents already
support targeted delivery. No provider permissions or model configuration change.

Selects are shadcn-templ `Select` via `selectField`, with server-rendered
options. Fragments containing Select/DropdownMenu/Dialog are replace-patched, or
option-keyed inside morph roots. No native-select fallback is provided. The
standalone recovery page still submits ordinary HTTP POST. Never call form.reset
on forms containing shadcn controls. Settings switches use state-keyed wrapper
IDs so changes to server defaults replace their mounted declaration.

## Compact UI regression fixture

Export the Board fixture and component fixtures above (including
`TestAgentsComponentFixture`), then run `JAWA_CARD_VISUAL_HTML=/path/cards.html
JAWA_COMPONENT_FIXTURE=/path/components.html npm run test:ui` in frontend.
This serves only real sanitized renderer fixtures/assets and simulated finite
SSE responses on a disposable loopback port. Chromium checks search geometry,
filter shortcuts, short project headings, Latest run metadata, primary contrast,
agent timestamps and record-provided labels after keyed replacement at
320/390/768/1024/1440px. Shared dialog failures retain drafts, clear on reopening,
and isolate late responses. Set `JAWA_UI_SCREENSHOTS=/path/images` for untracked
390/1440px screenshots covering Board/Archive search and project headings,
Planning Latest run and enabled primary controls, Agents before/after label
replacement, and every shared mutation dialog error. Geometry checks require
the project mark/title to share a row above Git metadata and a visible gap
between each dialog alert and title. Existing component/issue-recovery tests
remain independent.

### Planning run observation

`GET /runs/planning/{turnID}?tab=chat|trace` and
`GET /runs/planning/{turnID}/events?tab=chat|trace` reuse the authenticated,
no-store Chat/Trace viewer and shared BONNIE observation hubs. Unknown, unpinned
or malformed identities return 404; browser fields never choose a remote target.
The stream re-fetches planning storage and terminates if the pinned tuple changes
or the task disappears. Trace uses the same separate snapshot/durable/live API
source tables, not local planning receipts. Offline observations do not prevent durable chat acceptance;
missing fields/history are silently omitted rather than reconstructed.


Planning run viewers have the same durable chat composer, including offline and
historical runs. The full-card Discussion also offers that composer for the newest
pinned planning run when no typed planning reply is currently available; a pending
typed reply remains the sole authoritative reply path. Durable chat is projected
as ordinary user/assistant Discussion messages, deduplicated against typed
interaction messages; internal delivery classifications are not shown in Discussion. Chat POST uses the same CSRF/setup/auth guards as Build/Fix.
Planning Stop remains a card cancellation action. Specification approval requires
the explicit card control, not chat. Follow-ups cannot regain workflow authority.
Delivery status is separate from stored workflow status; failed/ambiguous messages
remain visible and never automatically acquire a new command ID. See README's
Run detail section for the v0.29 interrupted-input deviation and host protocol.
Board and
Archive planning status badges and View runs menus are native filtered-history
links; the card key/eye still opens the card. Planning execution history provides
Open run for each valid pinned task, including older sessions.
## Blocked full-card discussion checks

`/planning?card=<id>` is the full card view even without a planning session.
Accepted build/Fix blockers and saved guidance remain readable after delivery,
cancellation and restart, grouped by attempt following planning discussion.
Only the current pending interaction offers inline guidance; queued delivery
shows saved text immediately without allowing another reply.

From `internal/orchestrator/frontend`, run `npm run test:guidance`. It exports
real production markup with `JAWA_GUIDANCE_HTML` via `TestGuidanceBrowserFixture`
and tests the embedded bundle: success-only modal closure, rejected draft,
inline form payload/token, repeated live patch draft preservation, new-token
replacement, keyboard submit, and 320/390/1440px overflow. Set
`JAWA_GUIDANCE_SCREENSHOT_DIR` to an existing directory to capture Chromium PNGs.
Standalone fixture export from root:
`JAWA_GUIDANCE_HTML=/tmp/guidance.html go test ./internal/orchestrator -run '^TestGuidanceBrowserFixture$' -count=1`.
The `.pending`, `.queued` and `.next` suffixes are patch roots for this script.

## Auto merge controls

New card and all-status card detail share Use global and Auto merge switches. Only
`auto_merge_mode=inherit|on|off` is named; explicit Save uses authenticated, CSRF/setup
guarded `/cards/auto-merge` and expected-current-mode stale protection. Detail draft
forms ignore unsolicited morphs; successful saves reload saved state. New card reset
restores inherit; issue recovery retains the reserved preference. Merge evidence in
card detail is sanitized and separate from historical Ready/provider Merged badges.

Export `JAWA_AUTO_MERGE_HTML=$PWD/internal/orchestrator/frontend/node_modules/.auto-merge.html`
with `go test ./internal/orchestrator -run TestAutoMergeBrowserFixture -count=1`,
then run `npm run test:auto-merge` in frontend with the same absolute fixture path.
`JAWA_AUTO_MERGE_SCREENSHOTS` writes real Chromium mobile/desktop screenshots.
## Explicit merge browser fixture

Merge… appears only beside conflict-free non-archived Ready PR evidence; Archive,
unknown/checking/error/conflict/closed/merged/manual-merged/unresolved operations
have no enabled action. Dialog shows PR/base/head, offers Merge commit/Squash/Rebase,
and warns that dismissal cannot undo remote work. Cancel sends no mutation.

Export real templ fixtures and run scripted local responses (never a live PR):

```sh
JAWA_MERGE_HTML="$PWD/internal/orchestrator/frontend/node_modules/.merge.html" \
  go test ./internal/orchestrator -run '^TestMergeUIFixture$' -count=1
cd internal/orchestrator/frontend
npm ci && npm run build
npx playwright install chromium
npm run test:merge
```

The script checks strategy reset/retention, keyboard cancellation, duplicate
pending suppression, old-response isolation, no automatic retries, success
replacement and overflow at 320/390/768/1440px. Real Chromium screenshots are
written to `frontend/node_modules/.merge-screenshots/` (not committed).
Related regressions: `npm run test:pr-refresh`, `npm run test:fix`, `npm run test:ui`
(the latter requires the component/card fixtures documented above).
