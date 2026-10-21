# Datastar SSE views

The shared shell owns `@get('/events?view=…&project=…&state=…&card=…')`.
Validated viewParams scope both full rendering and SSE snapshots; unknown values
fall back safely. Forms submit hidden view/project/state/card fields alongside
CSRF and their normal named fields, so mutation responses patch the originating view.
Native forms redirect to the same canonical view (project creation goes to its board).

The authenticated stream rechecks session validity and snapshots once per second,
emitting changed HTML plus 15-second heartbeat comments. This is server-side
snapshot polling, not browser polling. Streams terminate on disconnect, revocation,
or shutdown. Write deadlines are refreshed to bound slow consumers.

Stable element patch roots:
- Full card: `planning-content` (`/planning?card=<id>`), including direct builds.
- Board: `board-content` (only the selected project's lanes/cards).
- Runs: `runs-content` (card/state filters, newest-first rows).
- Agents: `agents-content` (presence summaries and endpoints).
- Settings: static form content, deliberately not morphed.
- All screens: `sidebar-nav`, `agent-status`, and action-only `notice`.

Live updates omit notice; mutation success clears it or shows a success flash.
Expected failures return finite SSE alerts rather than navigation/raw SQL errors.
Dialogs are freshly server-rendered by authenticated `GET /dialogs/{name}` into
`#dialog-slot`, outside live patch roots. Every opening owns a random 32-hex
operation and frame `dialog-<op>`; errors target `<name>-error-<op>` only.
Successful mutation responses send snapshot and reset/navigation signals first,
then an operation-keyed remove frame **last**. Removing a frame unmounts its
component and cancels its request, so no response frames may follow it. Late
responses cannot remove another opening. Failed submissions preserve drafts.

Datastar owns signals and transport; shadcn-templ owns Dialog, DropdownMenu,
Select, Checkbox, Switch, CopyButton, Toggle, ScrollArea and MessageScroller behavior. Component
fragments use replace mode; selects inside live morph roots have option-keyed
IDs. Never morph a mounted declaration when its options change.

Sortable reports bubbling gesture events only. `#board-content` binds
`data-ignore-morph` to `$_dragging || $_menuOpen`; menu close without an action
requests a catch-up snapshot, while an action supplies its own authoritative
snapshot. Dialog acknowledgements and select defaults are fresh server markup.
Relative timestamps share one 15-second clock signal; shortcuts read shell
signals. Submit indicators disable buttons only, never named inputs.

Run details use `/runs/{attemptID}/events?tab=chat|trace`, with stable roots
`run-detail-content` and `run-detail-controls`. After the first full frame only
changed roots are sent (`run-detail-status`, `run-detail-controls`, `run-live`,
`run-pending`, `run-trace`); new chat entries use `mode append` into
`#run-messages` and changed entries morph by key. Draft, tab links, filter and
Trace source toggles are outside those roots. Session validity is rechecked each iteration (one-second
poll), with 15-second heartbeats and bounded writes. One upstream BONNIE stream
per agent/run is shared by viewers; each browser reads an immutable copied
observation without blocking upstream. Initial replay starts at zero for bounded
historical durable activity; subsequent reconnections use only the processed
durable cursor. Live equal-sequence events are not deduplicated. Snapshot
refreshes coalesce durable/StepFinish changes and replace covered live activity.
Closure silently retains observations and retries reads only with 1–30 second backoff.
Offline/capacity states do not authorize mutations. Notices/drafts are never
cleared by unsolicited patches. Send/Stop enhanced POSTs set retry:'never'.

## UI component ownership

- `layout.templ`: shared shell, sidebar, breadcrumb/header, agent chip, dialogs, auth.
- `board.templ`: selected-project kanban, compact cards and shadcn DropdownMenus.
- `runs.templ`: newest-first attempt list, state/card filters, expandable details.
- `agents.templ`: presence cards, endpoint table, inspection and connect empty state.
- `settings.templ`: agent credential rotation, connect instructions, provider status.
- `view.go`: validated routes/filters, presentation helpers; no secret values exposed.

Datastar owns requests and DOM patches; templ owns server-rendered markup.
All application reactivity is signal-driven; runtime helpers are component-owned.

- `run_detail.templ`: run-detail page, Chat/Trace history and separate controls;
  composer, filter and tab links are outside patch roots.
- `trace.templ`: Trace rows for agent steps, ported from shadcn's Trace.
- `run_detail.go`: pinned routing, state policy, finite actions and viewer SSE.
- `runapi.go`: signed HTTP-over-NATS requests and bounded shared observation hubs.
- `run_display.go`: ordered safe message/tool projection; BONNIE MIT attribution.

See CONTRACT.md for run-detail routes and build instructions.

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
Discovery is bounded; rendered choices contain only agent ID, provider,
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

Run detail silently omits empty sources/optional fields. Trace tables use API-only
snapshot/durable/live data, not local deliveries or inferred journal history.
Chat tool disclosures preserve open state. The textarea uses field-sizing
and signal-driven Ctrl+Enter eligibility and pending lock. MessageScroller follows
appended messages only at the bottom. A matching server confirmation clears the
draft; newer and uncertain drafts survive.

Runs snapshots and sidebar active counts use a separate mixed Planning/Build/Fix
read-only projection. Rows are keyed `run-{mode}-{local execution ID}`; replies
and resume keep the key, retries create another. Generic `/snapshot?view=runs`
and `/events?view=runs` include queued/unpinned and retained planning history.
Planning detail uses only `/runs/planning/{turnID}/events?tab=chat|trace`, with the
same bounded writes, authentication rechecks and identity-change fencing as build
details. Its controls remain observation-only without passive policy text; no planning composer
or Stop is rendered. Build authority collections and `/activity` are unchanged.
Guidance dialog success removes the correlated frame; inline guidance success
clears `guidanceReply`. Errors and live snapshots do not clear drafts. The
inline `/cards/guide` composer lives inside `planning-content`, with form and
textarea IDs keyed by report identity and `data-ignore-morph` draft preservation.
A new report replaces the old composer; queued replies render immediately with
no further composer. History is loaded only for the selected full card through
shared full-page/SSE loading. Run `npm run test:guidance` for production Go-markup
and embedded Datastar bundle regression checks.

## Merge confirmation

`POST /cards/merge` follows authenticated POST/CSRF/setup/operator admission and
native 303 view-preserving redirects. Enhanced requests use finite SSE, a keyed
`merge-error-{dialog_operation}` alert and `retry:'never'`. The shared merge dialog
lives outside live patch roots, resets its Select to Merge commit per opening,
locks pending submit and retains method/errors on refusal. Merge success closes
only the opening with its operation-keyed remove frame (not a global signal).
Closing an in-flight dialog aborts waiting, not remote work; uncertain outcomes
instruct Refresh ready PRs instead of claiming success. Live PR evidence patches
remove eligibility and the visible card Merge button when no longer clear.

### Component integration regressions

With pinned shadcn-templ beta.14, a dialog opened from a portaled menu does
not reliably restore focus to the card trigger. The approved temporary
workaround records the menu trigger id in UI-only `$_dialogReturn`; the
trigger's own delayed `dialog-close` listener focuses `el` after the dialog
exit. It performs no DOM query and adds no application JS helper.

Sortable's `board-move` event updates the move signals on the board. The shell
form posts on a one-tick delayed window listener, after Datastar's bound input
effects flush, so FormData contains the newly emitted card/lane/position.

MessageScroller owns `data-pending-scroll`: preserve that attribute on its root
and viewport during live morphs. Reapplying the server's initial pending marker
after initialization can otherwise hide a mounted transcript indefinitely.
