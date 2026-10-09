# UI SSE contract

- Same-package templ components: `boardContent(csrf string, projects []Project, cards []Card, attempts []Attempt)` emits top-level `#board-content`; `activityPanel(workers []presence.Record, attempts []Attempt, msg string)` emits top-level `#activity-panel`.
- Stable page root owns `data-init="@get('/events')"`, signals `filter`, `notice`, and `busy`, and `data-on:board-refresh="@get('/snapshot')"`. Neither root nor dialogs/settings/auth are included in board/activity morphs.
- Successful mutation responses patch both components and `notice` (empty string clears errors). Failures patch `notice` with an actionable message; UI does not navigate or poll. Form submissions use `@post(route, {contentType:'form'})`, including `_csrf`.
- Routes: `/projects`, `/cards`, `/cards/move`, `/cards/retry`, `/cards/reset`, `/cards/delete`, `/settings/nats`. Move form fields: `_csrf`, `card_id`, `status`, `position`.
- Drag temporarily sets `data-ignore-morph` on `#board-content`. On drop, optimistic Sortable movement is undone, protection removed, and a stable hidden form submits through Datastar. No-change drops request `/snapshot` via bubbling `board-refresh`. Worker activity remains live throughout.
- Global create-card dialog lives outside morph regions; project choices are copied from current project headings when opened, so newly created projects are immediately usable without replacing an open form.
- Authentication and logout remain conventional forms. Keep SSE authenticated and handle disconnect/reconnect through Datastar's transport.
