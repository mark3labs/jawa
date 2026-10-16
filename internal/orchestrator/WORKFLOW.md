# Workflow integration contract

`OpenStore` runs all database migrations. `NewWorkflow(ctx, store, conn) (*Workflow, error)` starts durable BONNIE v0.22 event/result readers and an outbox dispatcher. Caller owns Store and NATS connection: use the embedded server's trusted in-process connection, not public credentials. Close workflow before closing either dependency. Before dispatch, Workflow installs a Core NATS request/reply subscriber on `bonnie.reports.submit`. Close cancels and joins its goroutines (including report commits) and does not delete consumers.

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

Shared tasks use `bonnie.tasks`; explicit assignments use `bonnie.tasks.agent.<agentID>`. Root provisioning enables targeted subjects. Task UUID is deterministic from card ID and monotonically increasing attempt number. Persisted task payload and identity never change on publish retries. Broker ACK marks publication, not execution. Delivery is at least once, not exactly once outside the broker deduplication window; agents must deduplicate by task identity.

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

## Explicit agent assignment

Card menus offer **Plan with agent** and **Build with agent** in Todo,
**Build with agent** in Planning, and **Fix with agent** in Ready for review.
Todo builds start immediately without plan approval. Only generated plans need
review: Planning shows the rendered revision and one acknowledgement, then
**Approve & build with agent** freezes that exact revision in the existing
approval transaction. A stale revision is rejected; refresh the page to review
the new plan. Until a plan is ready, the dialog links to the planning view.
Fix requires a verified PR and a nonempty repair message, preserving its branch.
Existing shared-queue actions, drag/drop and ordinary Approve & build remain shared.

The selector lists connected agent IDs with advisory provider/model metadata and
availability. Busy input-capable agents are selectable; this is not a capacity
reservation or execution lease. Planning and build selections are independent.
The desired target is durable per task, separate from the observed run owner.
Publishing, restart recovery and fresh Retry retain it even after presence expires.
Queued targeted work waits for the selected stable ID within broker retention;
it never falls back to shared delivery or another agent. Replies, guidance,
cancellation and Resume still address the observed execution, not a new task.
Assignment fences admission but is not isolation against hostile trusted agents.

`GET /agents/choices` is authenticated, setup-guarded, GET-only and no-store.
Discovery is bounded to two seconds; the JSON contains only agent ID, provider,
model, availability and selectable capability. Targeted mutations revalidate
this projection before their atomic lane/task transaction. Shared actions do not
require presence. No browser-supplied subject, PR URL, branch or run selects work.
The assignment dialog stays outside SSE patch roots. It loads on open/Refresh,
aborts and invalidates stale reads, and retains drafts on failed submissions.
Successful `/cards/assign` responses clear only its assignment signals.
Run `npm run test:agents` in the frontend directory for selector race regressions.

Back up the database before migration 00019 and coordinate backend deployment.
Existing streams must include `bonnie.tasks.agent.*`; existing agents already
support targeted delivery. No provider permissions or model configuration change.

## Orchestrator-owned auto-merge

The lifecycle-cancelled serialized recovery pass considers at most 16 oldest-due
Ready candidates in 30 seconds after normal verification. Global `auto-merge`
defaults off, with live card inherit/on/off overrides. Latest normally verified
non-archived Done attempts only; active/pending-resume/conflicting authority,
Force-ready audit and applicable manual-merged markers are excluded. Full fresh
readiness binds the exact head to a durable reservation. No transaction spans
provider I/O. Lane/delete/archive mutations are fenced while reserved/inflight;
policy disabling remains allowed and invalidates undispatched reservations.
Dispatched writes may still succeed after disabling. The ledger retains that
outcome, separately from Ready. Interrupted reservations become uncertain on
restart and reconcile read-only before safe fresh-verification retry. Persisted
backoff is one minute exponentially up to 30 minutes (longer Retry-After honored).
There is no cross-provider exactly-once guarantee; one orchestrator per database.
## Operator PR merge reservations

`MergePR` admits only a non-archived Done card's latest ready PR with applicable
clear head/base evidence, no manual merged marker, active attempt or pending
resume. A rendered snapshot hashes current attempt/project configuration and
observed revisions. An opaque operation ID durably binds card/snapshot/method;
exact repeats return recorded outcomes, never replay provider writes. A short
write transaction reserves one unresolved operation per card before provider I/O.
Fix (including assigned Fix), delete/reset, archive and local Mark as merged share
the reservation guard. No transaction spans network I/O.

`pr_merges` retains sanitized pending/merged/rejected/uncertain audit and operator
identity without tokens or raw provider responses, even after card deletion.
Restart marks pending reservations uncertain. Poll/explicit refresh perform only
read reconciliation after the 25-second reservation window; unknown/failed checks
retain uncertainty. Fresh explicit confirmation is required after rejection.
Finalization attaches merged evidence only to matching latest attempt/configuration;
lane, ordering, readiness, manual provenance and archive state are unchanged.
Poll evidence uses durable operation generation/status predicates, not mutexes
alone, so checks begun before reservation cannot overwrite final evidence.
Provider writes and SQLite cannot commit atomically; no exactly-once claim is made.

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

## Remote MCP lifecycle

MCP lifecycle tools call the same Workflow methods as dashboard mutations, with
identical revision, interaction, resume and auto-merge CAS tokens. They add no
new transitions: writes queue durable work or record a decision and return a
refreshed card; clients follow progress through get_card/wait_for_card. Creation
shares transactional insert helpers and the dashboard's issue submission ledger.
