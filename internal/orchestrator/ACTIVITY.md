# Runs and agents

Runs (`/runs`) and Agents (`/agents`) are separate authenticated views, not sections
beneath the kanban. Runs can be narrowed by state and card. Agents show availability,
current work, last update, channel endpoints, and optional raw-record inspection.

Each view subscribes to its scoped Datastar SSE stream. A live `agent-status` chip
and sidebar counts appear everywhere. Board cards link to detailed Runs rather than
expanding logs in the board. Provider token configuration is shown as boolean status
in Settings only; credentials never enter these snapshots.

GET /activity remains available for JSON inspection/integration assertions, but the
browser UI does not poll it. See SSE_UI.md for scoped patch roots and lifecycle.
