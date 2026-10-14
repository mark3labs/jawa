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

Project badges and the board's **active** total count cards with active planning,
building, or fixing work, including queued/submitted work and pending resumes.
Archived cards are included because archiving does not stop execution. The tooltip
and accessible label break down these modes. Paused/input-waiting, reconciliation-
blocked, and terminal work are excluded (a planning reply pending delivery counts
again). This is advisory durable work state, not a count of live agent processes.
At zero the sidebar badge is hidden; the board still shows `0 active`.

Runs includes **Planning**, **Build**, and **Fix** executions from every retained
session, newest first (creation time, then mode and local ID). A queued planner
appears immediately without an Open link until its agent/run/remote-attempt tuple
is pinned. Replies, revisions and resumes retain the same task row; retries add
history. Approval queues a separate Build row. Cancellation and archiving retain
history; deleting a card/project still cascades its execution history.

Active includes queued/submitted/running work and pending resumes/queued replies,
including unfinished post-approval planners. Blocked includes planning input,
blocker guidance and specification review; Failed includes failed turns. Ready
is implementation/PR readiness only, never specification readiness. Each planner
uses its own interaction and execution evidence, not the card's newest session.
`/activity` retains its existing build-only `attempts` JSON contract.
