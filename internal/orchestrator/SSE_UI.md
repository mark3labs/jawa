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
Shared mutation dialogs own `project|card|delete|fix|guide|cancel|ready|force-error`
roots, outside live view roots. Each opening assigns a validated opaque operation
suffix to its error root and hidden form transport field. Only fixed server-owned
POST routes select a destination; messages remain templ-escaped. Non-dialog
actions retain the page notice fallback and native failures retain HTTP statuses.
Failures leave the modal open and preserve drafts/acknowledgements. Live snapshots
omit these roots. Fresh open and dismissal clear local errors; dismissal also
aborts that opening’s request, so delayed errors and success signals cannot
contaminate another operation. Success clears the correlated alert, marks form
reset evidence and closes via existing signals. Issue creation keeps its dedicated
recovery controls and explicit retry policy.
Dialog-close signals accompany success. Project creation patches `goto` with a
server-generated same-origin board URL; the stable shell navigates through a
Datastar effect. No script strings from user input are executed.

Dialogs are outside patch roots, preserving drafts. Run disclosures preserve `open`.
Rocket wraps SortableJS; entire noninteractive cards are draggable. During a drag,
board-content uses data-ignore-morph. Drop restores authoritative layout before
submitting a hidden Datastar form; unchanged drops request a finite snapshot.

Rocket also wraps card popovers, keyboard shortcuts, copy controls and relative
timestamps. Prebuilt shadcn-templ components retain their markup and styles. See
CONTRACT.md for builds and integrated browser smoke instructions.

Run details use `/runs/{attemptID}/events?tab=chat|trace`, with stable roots
`run-detail-content` and `run-detail-controls`. Draft, tab links and filter are
outside those roots. Session validity is rechecked each iteration (one-second
poll), with 15-second heartbeats and bounded writes. One upstream BONNIE stream
per agent/run is shared by viewers; each browser reads an immutable copied
observation without blocking upstream. Initial replay starts at zero for bounded
historical durable activity; subsequent reconnections use only the processed
durable cursor. Live equal-sequence events are not deduplicated. Snapshot
refreshes coalesce durable/StepFinish changes and replace covered live activity.
Closure shows unreachable and retries reads only with 1–30 second backoff.
Offline/capacity states do not authorize mutations. Notices/drafts are never
cleared by unsolicited patches. Send/Stop enhanced POSTs set retry:'never'.

## UI component ownership

- `layout.templ`: shared shell, sidebar, breadcrumb/header, agent chip, dialogs, auth.
- `board.templ`: selected-project kanban, compact cards and Rocket action popovers.
- `runs.templ`: newest-first attempt list, state/card filters, expandable details.
- `agents.templ`: presence cards, endpoint table, inspection and connect empty state.
- `settings.templ`: agent credential rotation, connect instructions, provider status.
- `view.go`: validated routes/filters, presentation helpers; no secret values exposed.

Datastar owns requests and DOM patches; templ owns server-rendered markup. Rocket's
light-DOM setup/cleanup encapsulates gestures, menus, shortcuts, relative time and
copy behavior. Stable keyed roots keep the shell, filter and drafts intact. No manual
fetch loop or page reload is used for card actions.

- `run_detail.templ`: run-detail page, Chat/Trace history and separate controls;
  composer, filter and tab links are outside patch roots.
- `run_detail.go`: pinned routing, state policy, finite actions and viewer SSE.
- `runapi.go`: signed HTTP-over-NATS requests and bounded shared observation hubs.
- `run_display.go`: ordered safe message/tool projection; BONNIE MIT attribution.

See CONTRACT.md for run-detail routes and build instructions.

Runs snapshots and sidebar active counts use a separate mixed Planning/Build/Fix
read-only projection. Rows are keyed `run-{mode}-{local execution ID}`; replies
and resume keep the key, retries create another. Generic `/snapshot?view=runs`
and `/events?view=runs` include queued/unpinned and retained planning history.
Planning detail uses only `/runs/planning/{turnID}/events?tab=chat|trace`, with the
same bounded writes, authentication rechecks and identity-change fencing as build
details. Its controls root explains observation-only policy; no planning composer
or Stop is rendered. Build authority collections and `/activity` are unchanged.
Guidance success signals close `guideOpen` and clear `guideCard`, `guideToken`
and `guideMessage`; error notices and live snapshots do not clear them. The
inline `/cards/guide` composer lives inside `planning-content`, with form and
textarea IDs keyed by report identity and `data-ignore-morph` draft preservation.
A new report replaces the old composer; queued replies render immediately with
no further composer. History is loaded only for the selected full card through
shared full-page/SSE loading. Run `npm run test:guidance` for production Go-markup
and embedded Datastar bundle regression checks.
