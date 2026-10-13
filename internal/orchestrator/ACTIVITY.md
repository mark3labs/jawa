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

## Project active work

The amber project badge and the board's **active** total count cards with active
planning, building, or fixing work, including queued/submitted work awaiting
execution and pending resumes. Each card counts once. These project-wide counts
include archived work: archiving hides a card, but does not stop its execution.
The accessible label and tooltip provide the mode breakdown and these inclusions.
At zero the sidebar badge is omitted; the board still displays **0 active**.

Counts use the current planning session's newest turn or the latest build/fix
attempt, not lane occupancy or historical runs. Paused work awaiting input,
interrupted/blocked work, reconciliation conflicts, and terminal work are excluded
unless explicitly awaiting resume confirmation. Reply delivery pending in a
running planning session counts as active. This is advisory durable work state,
not a count of agents executing on a CPU or proof of remote cancellation. Lane
counts, the ready statistic, and the separate Runs count retain their meanings.
