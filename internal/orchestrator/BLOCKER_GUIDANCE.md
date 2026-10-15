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
   Successful enhanced submission closes the guidance dialog and clears its
   signals only after persistence; rejected/stale submissions keep the draft.
   This acceptance is not confirmation that the agent has consumed the reply.
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

## Full-card discussion

`/planning?card=<id>` is the full card view, including cards built directly from
Todo. Discussion shows existing planning discussion first, then accepted typed
build/Fix blockers paired with nonempty operator replies in attempt/report order.
Identical reasons remain separate messages keyed by report identity. Legacy
blockers without interactions are read-only; answered and cancelled exchanges
remain visible across reload/restart. Operational errors, proposals, initial
descriptions and arbitrary agent output do not become discussion messages.
All messages use the shared sanitized Markdown renderer.

The latest pending Building/Fix interaction offers an inline **Reply with
guidance** composer posting the same `/cards/guide` endpoint and exact report
ID as the dialog. Drafts survive live patches for that interaction; a later
blocker replaces both form and textarea identities. Guidance is persisted and
shown immediately while queued, with no second composer until a new blocker.
Cancellation-requested, ready, running and closed interactions cannot invite
replies. Historical messages never authorize guidance; server CAS remains
final authority for stale forms/races. No new execution attempt is created.

No precise reply timestamps are available: replies are displayed beside their
blocker, not as a globally timestamp-sorted chat transcript. History is loaded
only for the selected full-card view, not every board snapshot.

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
