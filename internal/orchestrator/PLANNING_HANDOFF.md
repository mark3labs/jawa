# Resumable planning backend/UI contract (v7)

## Workflow and UI

- `MoveCard(cardID, "Planning", position)` creates one durable task with read-only investigation instructions. Todo → Building remains optional.
- `PlanningDetail` includes typed `Questions`, `InteractionToken`, interaction statuses, immutable revisions, discussion and execution history. Clarify does not create a spec/revision. Questions and review are separate states.
- `ReplyPlanning(cardID, text, expectedInteraction)` requires the exact displayed report token. The endpoint reads hidden `expected_interaction`; missing, consumed, cancelled or stale tokens reject. It records the user message and immutable reply outbox before dispatch. It never creates a task/run/attempt.
- `ApprovePlanning(cardID, expectedRevision)` guards the card-global monotonic revision, pending review and session state. One transaction freezes approved spec/revision, queues an approved Answer, creates a DISTINCT implementation task and moves the card to Building. The planning run resumes explicitly and ends naturally; its completion/failure cannot mutate the approved snapshot or build.
- `CancelPlanning` revokes local authority, closes pending/queued interactions and moves to Todo atomically. Published investigations retain an exact-owner cancellation queue; remote silence does not prove execution stopped.
- `RetryPlanning` creates fresh work only after failure, not on a normal reply/revision.

Reply forms are keyed by interaction identity so SSE cannot retain an old hidden token for a new question. Approval selections refresh with the proposal; server CAS remains authoritative. Discussion content is sanitized Markdown from typed reports, never parsed natural model text. Planning report statuses distinguish pending input, queued delivery, dispatched reply and closed interactions.

## Protocol, runtime and persistence

`clarify(questions)` requires 1–30 nonempty questions, no spec/commit.
`proposeSpec(spec, repo_commit)` accepts NO questions. It waits for review.
Accepted planning reports are durable interaction commits, not terminal task reports. Trusted identity includes task/agent/run/attempt/turn/**tool_call_id**. Exact receipt replay checks the entire original identity before runtime checks, including after supersession/deletion. New reports require current authority and BONNIE running-turn observation. Build still requires typed completeTask with a positive PR, followed by existing provider verification.

Migration 7 replaces the task-primary-key receipt table with report-primary-key and `(task_id,tool_call_id)` uniqueness; a partial unique index permits one terminal complete/blocker per task. Planning interactions allow only one pending/queued report per task. Receipts remain independent of card deletion; interaction/outbox rows cascade with planning history. sqlc schema/models/queries are regenerated with v1.30.0.

An interaction ACK can arrive before BONNIE writes RecordSuspend. User input may be queued immediately but cannot publish until an exact matching waiting outcome is persisted: task/agent/run/attempt, suspension TurnID, ToolCallID, version, error-free state and configured agent answer route. Dispatch calls BONNIE `Answer` with that original outcome and response TurnID/Text/Approved. Broker ACK marks dispatch, NOT model consumption. Failed/offline dispatch leaves the immutable outbox queued. BONNIE deduplicates answers by route/run/tool identity. Bridge intent/receipt recovery repairs the accepted-report-before-suspend crash window; runtime SQLite journal retains context and suspension across restart. Startup replays retained waiting outcomes for ready/questions/running and approved sessions. Waiting never becomes planning failure; unexpected natural completion before approval does.

While an intent is unconfirmed, identical-payload tool retries reuse the original
complete identity and report ID; changed payloads reject. Once a receipt resolves
an interaction, later identical planning calls are distinct interactions. Restart
recovery replays original intents before model prompting.

Approval and build can run concurrently after commit: neither completion ordering nor a shared provider script may be assumed. Approved planning finish is bookkeeping only. Planner prompt instructs natural acknowledgement after approval (completeTask requires a PR and is not a zero-PR planning completion API).

## Deployment and limits

Protocol version remains 1 for coordinated source deployment; old binaries without tool_call_id are incompatible. Stop old agents before upgrading. Migration retains v6 receipts verbatim with synthetic legacy ledger keys. Old ready/questions sessions had terminal remote runs, not suspensions: history/specs remain but sessions become failed with an explicit retry message. Do not invent an Answer for them. Approved/build history is untouched.

Read-only scope is prompt-level; deploy restricted credentials/tools/sandbox for actual enforcement. Agent repo_commit is reported, not independently attested, and subsequent proposals must match the first accepted SHA. Recovery is bounded to the newest 10,000 retained results/5 seconds; provision broker retention accordingly. The shared authenticated agent credentials are a trust boundary, not hostile-agent isolation.

## Verification

- Real KIT testmodel + BONNIE runtime + SQLite journal + Bridge + local JetStream + production backend DB ACK: clarify → restart both owners → answer on same run → propose → reject/revise → propose → approve → natural planning completion plus distinct typed build.
- Reply-before-suspend race, wrong waiting identities/route, disconnected reply outbox/replay, stale/double replies, cancellation, unexpected completion, exact receipt replay and v6→v7 migration constraints.
- Full-cycle Playwright factory smoke with real host and backend: Questions without proposal, answer, proposal, revision, approval, separate build; existing desktop/mobile/SSE/presence/verification smoke retained.
- Full Go suite/race suite, vet, golangci-lint. These offline checks do not exercise paid providers or live repository writes.

## Durable run chat

All retained pinned Planning/Build/Fix run detail pages accept durable messages,
regardless of lane or owner availability. Migration 00022 stores per-run client
message identities, pinned command identity, classification and delivery status.
Acceptance is not delivery. The bounded ordered dispatcher retries only the same
command; unreachable/old agents remain visibly pending. Deploy updated owners and
orchestrator together (BONNIE v0.29, `jawa.run_chat=1` capability).

Current pending guidance/clarification/review shares the exact-interaction Answer
outbox and CAS with card forms. Review chat is revision guidance, never approval.
Already queued card replies win; chat waits and reclassifies, never overwrites.
Current explicit chat excludes automatic/manual failed resume reservations.
Historical/readied/abandoned/approved-planning messages are conversational follow-ups:
host-journal provenance and orchestrator fences deny workflow evidence, including
late reports, status, results and resume receipts. No new attempt or PR readiness.

Explicit input on an idle interrupted run cancels only that observed old turn
before Start, preserving conversation/sandbox but ending the old completion budget.
This avoids silently recovering old completion work instead of consuming new text.
Retired/missing conversations fail without replacement. Orphan host intent is
ambiguous, not permission to run again; subsequent commands stay pending until
safe operator reconciliation. No automatic new-ID replay or external exactly-once
claim. Original failure evidence is retained until host-confirmed continuation.
See README Run detail for compatibility, security and operational limitations.
