# Blocker guidance

An agent should never be permanently blocked. `reportBlocker(reason)` therefore
*parks* the current run for operator guidance instead of ending it, and the
operator's reply continues the **same** BONNIE run, conversation, task ID,
remote attempt, sandbox and branch. Nothing is re-published and no new attempt
or branch is allocated.

## Protocol

The orchestrator decides, when it commits the report, whether a blocker awaits
guidance, and returns that decision in the post-commit ACK as
`await_guidance: true` (`reportprotocol.Ack.AwaitGuidance`). The decision is
durable: duplicate deliveries return the same value.

- Build (Building/Fix) blockers always await guidance.
- Planning blockers await guidance before approval. After approval nobody reviews
  planning, so a planner's closing blocker stays terminal bookkeeping.

The agent bridge maps an `await_guidance` blocker to a BONNIE
`SuspendRequest{kind: question}` (exactly like `clarify`), so the run enters
`waiting`. `restorePendingSuspensions` repairs the ACK-before-RecordSuspend crash
window for blockers too. Without the flag (older orchestrators) the blocker stays
terminal and the run completes, as before.

Operator guidance is delivered with BONNIE `Answer`, bound to the suspension's
turn. The model sees its blocker tool result followed by the guidance as the next
user message.

## Orchestrator state

`workflow_interactions` is the build guidance outbox (`planning_interactions`
already serves planning). A blocker with an interaction row is not a terminal
report; blockers without one (legacy or post-approval) still are. The terminal
unique index covers only `complete_task`, so a task may block several times.

While a blocker is open the attempt is `state=blocked`, `run_state=waiting`, with
the reason as its error. Late running/waiting status events, resync snapshots and
result replays keep that presentation; replays of an already answered suspension
are ignored. Typed reports are refused while a blocker is open.

1. **Reply with guidance…** (`POST /cards/guide`, `GuideBlocked`) queues the reply.
   The form carries the blocker's report ID; stale or repeated replies are rejected.
2. The dispatcher delivers queued guidance once the trusted waiting outcome
   (answer route and turn) is stored, retrying across restart. Delivery marks the
   interaction answered and the attempt running.

Planning blockers move the session to *Needs input*; the planning page shows the
blocker and the existing reply form delivers guidance through the same path as
clarification answers.

## Abandoning a blocked run

A parked run is active, so fresh Retry and Reset are refused until it stops. Use
**Cancel run…**; the confirmed cancellation closes the open interaction and makes
Retry/Reset available. A blocked run that ends instead of parking (an agent
without this change) keeps its blocker as a terminal reason and is retryable.

## Compatibility

Deploy agents and orchestrator together. A new agent with an old orchestrator
keeps terminal blockers. An old agent with a new orchestrator completes the run;
the orchestrator then closes the interaction and treats the blocker as terminal,
so the card is never stuck waiting for guidance it cannot deliver.
