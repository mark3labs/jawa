# Datastar SSE UI integration

`renderFragments(ctx, store, workflow, csrf)` returns board, activity, and empty
notice components. `Snapshot(request, store, workflow)` renders their HTML.
`app.snapshot` owns finite responses; `app.events` owns the live transport.

The stable page shell initializes `@get('/events')`. The authenticated stream
rechecks session validity and snapshots once per second, emitting changed DOM
fragments plus 15-second heartbeat comments. This is server-side snapshot polling,
not browser polling. Disconnects and session revocation terminate the stream.
Write deadlines are refreshed for bounded, long-lived responses.

Datastar morphs `board-content`, `activity-panel`, and `notice` by ID using
`datastar-patch-elements`. Unsolicited updates omit notice so action failures remain
visible. Mutation success clears notice and patches dialog-close signals; failure
patches an escaped notice without closing dialogs or navigating away.

Forms use `data-on:submit__prevent="@post('/…', {contentType:'form'})"` and submit
`_csrf` plus named fields. Native authentication/logout still navigate normally.
Dialogs live outside patched board/activity roots, preserving drafts. Disclosure
panels use `data-preserve-attr="open"` to preserve expansion during morphing.

Sortable handles gestures only. A stable hidden form submits moves through Datastar.
During drag, board-content uses `data-ignore-morph`; rejected or unchanged moves
keep authoritative layout. A no-change drop requests finite GET /snapshot.

The UI uses prebuilt shadcn Card, Badge, Input, Button, Label, Textarea, Alert,
Separator and dialog primitives. Native selects preserve form semantics without
requiring component registry scripts. CSS is compiled through Tailwind with Nova
styles; generated CSS/JS and templ output are intentionally embedded and committed.
See CONTRACT.md for regeneration and browser smoke instructions.
