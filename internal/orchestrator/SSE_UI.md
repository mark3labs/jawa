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
- Board: `board-content` (only the selected project's lanes/cards).
- Runs: `runs-content` (card/state filters, newest-first rows).
- Agents: `agents-content` (presence summaries and endpoints).
- Settings: static form content, deliberately not morphed.
- All screens: `sidebar-nav`, `agent-status`, and action-only `notice`.

Live updates omit notice; mutation success clears it or shows a success flash.
Expected failures return finite SSE notices rather than navigation/raw SQL errors.
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
