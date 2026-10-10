# Planning backend / UI handoff (API fixed)

Workflow methods:
- `MoveCard(cardID, "Planning", position) error`: Todo starts a durable investigation; same-lane reorder supported. Todo -> Building remains optional.
- `PlanningDetail(cardID string) (PlanningDetail, error)`; `Store.PlanningDetail` also available for offline display. No session returns sql.ErrNoRows.
- `ReplyPlanning(cardID, text string) error`: records user message, resolves outstanding questions, queues a new investigation turn. Rejects active/approved/cancelled sessions.
- `RetryPlanning(cardID string) error`: queues a new turn only after a failed turn.
- `ApprovePlanning(cardID string, expectedRevision int) error`: revision CAS; rejects active turns, questions, absent spec; atomically freezes approved spec and queues Building implementation.
- `CancelPlanning(cardID string) error`: invalidates session/turns and moves back to Todo; late deliveries cannot revive it.

Types (exported fields): `PlanningDetail{Session PlanningSession, Messages []PlanningMessage, Revisions []PlanningRevision, Turns []PlanningTurn}`; Session has ID/CardID/State/Repo/BaseBranch/Provider/RepoCommit/Spec/Error string, Revision/ApprovedRevision int, ApprovedSpec string, CreatedAt/UpdatedAt time.Time. Messages have ID/SessionID/Role/Content string, Sequence int, CreatedAt time.Time. Revisions have SessionID/Spec/RepoCommit string, Number int, Questions []string, CreatedAt time.Time. Turns have ID/SessionID/State/Error/Result string, Number int, Published bool, CreatedAt/UpdatedAt time.Time. Detail has `Questions []string` (latest revision unresolved questions), `CanApprove bool` convenience.

Session states queued/running/ready/questions/failed/approved/cancelled. Turn states queued/submitted/running/completed/failed/cancelled. Poll detail/SSE as with other workflow state. Render all agent content escaped/untrusted. Approval form must submit the displayed Session.Revision; do not fetch latest on submit.

Planner protocol: BONNIE v0.22 typed Task/Outcome over existing bonnie JetStream. Each conversation turn is a new deterministic durable task (full immutable repository/card context + conversation + previous spec); terminal typed `proposeSpec(spec, questions, repo_commit)` submission. No PR verification. Native waiting is not used for conversation: planner must submit questions through proposeSpec. User reply queues another independent task, avoiding non-durable remote resume. Read-only investigation is explicitly scoped in prompt; deployment must give planner read-only repository credentials/tool sandbox (BONNIE wire Task has no tool capability restriction). Persistence and delivery guards protect against stale sessions, duplicate results and mismatched remote identity.

## Implementation / verification update

Backend APIs are implemented and `TestPlanning*` passes, including durable database reopen, actual BONNIE JetStream task publication and typed proposal persistence and workflow restart, conversation questions/reply/revision, CAS approval races, cancellation/new-session late result guards, pinned commit rejection, natural outcomes without reports, atomic rollback, and approved-spec preservation on build retries. `go test ./...` passed after UI integration; planning race tests also pass.

Important: revision numbers are **card-global monotonic tokens**, not restarted at 1 per session. This prevents an old approval form from a cancelled session approving a new session with the same revision number. Session-local revision lists can therefore begin at e.g. 3; display Number verbatim.

Cancellation returns the card to Todo immediately and invalidates queued/running planner authority. Published tasks receive best-effort exact-turn BONNIE cancellation via a durable local retry queue; timeout/offline agent does not imply remote stop. Late event admissions are pinned solely for cancellation. All raw planning deliveries (including duplicate/conflicting/late results) are retained in planning_deliveries; no late output may change a cancelled/approved/superseded session. Native BONNIE waiting/interrupted outcomes fail this planner contract and are retried as fresh tasks; UI user conversation uses persisted typed proposal questions, not remote ask-user suspension.

Read-only scope is prompt-level in this integration because BONNIE v0.22 Task exposes only Text/Context and no tool allowlist. Enforce real isolation in the planner agent's credentials/tool configuration; do not mistake the prompt for a security sandbox. Typed repo_commit is reported by agent, not independently attested. Sessions pin the first accepted full SHA, and subsequent typed proposals must match it. Natural event/outcome evidence recovery is bounded to the latest 10,000 messages/5 seconds (same operational limit as existing build workflow); broker retention must cover outages.

## Typed backend authority (current)

The natural text parser and all `planning_output*.go` parser tests have been
removed. Only typed proposeSpec/reportBlocker submissions mutate planning.
`proposeSpec` validates canonical spec/questions/full SHA via reportprotocol and
pins the inspected commit. The proposal, revision, readable Markdown discussion,
turn/session transition and durable report receipt commit together before ACK.
Empty questions are persisted as `[]`; unresolved questions prevent approval.
Reply queues a new independent task; card-global revision CAS still protects
approval. New reports require exact current task/run/attempt/turn verified by
BONNIE runtime Status; duplicate replay matches the original stored complete
identity before active/current checks. Cancelled/superseded reports reject.
Native outcomes remain audit/status evidence, never proposal authority. Finishing
without an accepted report fails clearly. Accepted tools halt only after DB ACK,
so completion cannot race ahead of a committed typed proposal.

See WORKFLOW.md and ../../TYPED_REPORT_HANDOFF.md for shared receipt semantics.
Production offline browser smoke now passes with actual typed completeTask tools
and durable backend acceptance. No live repository/provider writes are claimed.
