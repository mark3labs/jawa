# Workflow integration contract

`NewWorkflow(ctx, store, conn) (*Workflow, error)` migrates workflow tables and starts durable BONNIE v0.22 event/result readers and an outbox dispatcher. Caller owns Store and NATS connection: use the embedded server's trusted in-process connection, not public credentials. Close workflow before closing either dependency. Before dispatch, Workflow installs a Core NATS request/reply subscriber on `bonnie.reports.submit`. Close cancels and joins its goroutines (including report commits) and does not delete consumers.

Route board mutations through `Workflow.MoveCard(id, status, position)` instead of Store.MoveCard. Todo -> Building atomically snapshots the task, inserts a queued attempt, and reorders lanes in one SQLite write transaction. Reordering Building does not create another attempt. Leaving Building is refused; `CancelCard(ctx, id)` requests cancellation only after confirming the latest attempt's exact agent/run/remote-attempt identity and active turn. A request is not confirmation: only an agent-confirmed cancellation is applied as cancelled, and cancellation cannot undo external actions already taken. Ordinary manual Done is forbidden. The sole exception is authenticated,
explicitly acknowledged `ForceReady`: an inactive latest unready Building/Fix
attempt with no pending resumes, older active attempts or execution conflict can
be bound to a server-selected PR after identity-only provider validation. A
rendered snapshot/configuration token is required and rechecked in the final
transaction. Provider failure rejects the action; CI, reviews, draft and
mergeability checks are deliberately bypassed. Provider I/O runs before the write
transaction, with cancellation and a 20-second deadline. The transaction stores
the full prior attempt plus operator/reason/head/time audit, sets Ready, moves to
Done, and clears auto-resume schedules and queued guidance without changing
outcomes or typed reports. Ready fences late deliveries. Exact duplicate tokens
preserve the original audit/time; repairs and configuration changes cannot reuse
them. No provider mutation, merge or agent command is performed. Store's legacy API is unchanged and must not be used to bypass these rules.

Tasks use shared `bonnie.tasks`; root provisioning enables targeted subjects for future use. Task UUID is deterministic from card ID and monotonically increasing attempt number. Persisted task payload and identity never change on publish retries. Broker ACK marks publication, not execution. Delivery is at least once, not exactly once outside the broker deduplication window; agents must deduplicate by task identity.

`Attempts() ([]Attempt, error)` lists history; `CardResult(id) (Attempt, error)` returns latest attempt; `Ready(id) (bool, error)` exposes verified readiness. Agent/run/remote-attempt identity is pinned by the first event or result: competing agent executions cannot overwrite it. Events never establish success. Only `completeTask(pr_number)` and `reportBlocker(reason)` typed tool submissions establish build reports. The versioned `internal/reportprotocol` request is validated independently, including its canonical payload hash, exact pinned task/agent/run/attempt ownership, current card/attempt, and active BONNIE Status runtime observation with matching turn. A missed admission event can be resolved by that exact runtime observation. No identity comes from model arguments. Natural completed outcomes without an accepted typed report fail clearly; natural text is never parsed or used as fallback. Accepted terminal reports cannot be overridden by natural outcomes, status events, reconciliation or resync. A `reportBlocker` committed as awaiting guidance is not terminal: it parks the run until operator guidance is delivered (see BLOCKER_GUIDANCE.md). Result and OutcomeJSON contain the typed request for accepted reports.

Migration 7 extends the durable `typed_reports` inbox to report ID and task/tool-call identity, with one terminal report per task and a durable planning reply outbox. Inbox receipt and planning/build transition share one SQLite transaction; Accepted ACK is sent only after commit. Exact retries return Duplicate only for the original complete identity, including turn, even after completion, recovery, supersession or card deletion. Different payloads or owners conflict. Report IDs do not encode identity, so the owner check is mandatory. New submissions require active/current authority. BONNIE durable tool intents replay their original identity after a lost reply; broker publication ACK is not a database commit. The shared agent credentials trust boundary is not hostile-agent isolation.

`SetVerifier(Verifier)` installs `func(context.Context, Attempt) (bool, error)`. It must independently verify repository/branch, actual PR existence, CI and required reviews; agent claims alone are not evidence. Callback receives immutable task context in TaskJSON plus the PR URL derived from the project’s configured provider and repository. Without a verifier, completed attempts are blocked awaiting verification. Verification is retried periodically, including after restart; false/error keeps Building. Only true from a verifier transactionally sets ready and moves the card to Done, preserving lane ordering. Callbacks must honor context cancellation and be safe to repeat. The existing provider adapter independently verifies readiness.

`Agents(ctx) ([]presence.Record, error)` discovers live records in `jawa_agents` (30-second TTL, create enabled). Presence is advisory, never a prerequisite for queued shared work. BONNIE's API is Discover, not List. Durable event/result consumers are suffixed with a stable database-owned identity so independent boards do not compete for deliveries. Startup and periodic resync recover retained status history to discover missed execution identities. Unknown tasks are acknowledged; database errors cause redelivery. Poison/invalid protocol messages are retained by BONNIE and retried, not silently discarded.

## Resumable planning

Clarify persists questions only and suspends; proposeSpec persists spec/commit
only and suspends for review. Replies require the rendered interaction token;
Answer waits for the exact trusted suspension and preserves task/run/attempt and
context. Approval atomically freezes the revision, queues approved Answer, and
creates a distinct build attempt. Natural waiting is expected, and natural
completion after approval cannot mutate build authority. See PLANNING_HANDOFF.md
for outbox/restart/cancellation semantics and coordinated v7 deployment.

## Failed execution continuation

`ResumeFailed(cardID)` and `ResumeFailedPlanning(cardID)` reserve a same-run
continuation for an owned execution failure only. UI routes require the rendered
failure-generation token. Fresh Retry remains separate. See [FAILED_RESUME.md](FAILED_RESUME.md)
for the verified BONNIE APIs, JAWA host control protocol, durability and ambiguity limits.
