# UI component ownership

- `layout.templ`: shared shell, sidebar, breadcrumb/header, agent chip, dialogs, auth.
- `board.templ`: selected-project kanban, compact cards and Rocket action popovers.
- `runs.templ`: newest-first attempt list, state/card filters, expandable details.
- `agents.templ`: presence cards, endpoint table, inspection and connect empty state.
- `settings.templ`: agent credential rotation, connect instructions, provider status.
- `view.go`: validated routes/filters, presentation helpers; no secret values exposed.

Datastar owns requests and DOM patches; templ owns server-rendered markup. Rocket's
light-DOM setup/cleanup encapsulates gestures, menus, shortcuts, relative time and
copy behavior. Stable keyed roots keep the shell, filter and drafts intact. No manual
fetch loop or page reload is used for card actions. SSE_UI.md documents wire behavior.
