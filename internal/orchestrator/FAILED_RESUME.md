# Resume failed run

Build and Fix card menus and failed planning details offer **Resume failed run** separately
from fresh **Retry attempt / Retry planning**. Resume keeps the task ID, board
attempt/planning turn, remote attempt, RunID, saved conversation/context and assigned
branch. It does not publish the original task again, allocate a worktree, or append
the original prompt again. Retry retains its existing fresh-attempt behavior
(including a new build branch).

Eligibility is intentionally narrow: latest execution on its current Building, Fix or
Planning card, a versioned natural `failed` outcome with complete pinned ownership,
no accepted terminal typed report, no active work or pending resume, and no retained
competing execution. Explicit reportBlocker, completed-without-report, cancelled,
waiting, interrupted, legacy and ambiguous executions cannot use this action.
Planning review/clarification still uses Answer, not failed-run resume. Rendered
forms carry a failure-generation token; stale forms are rejected.

Before reserving work, the orchestrator queries the exact owning agent's BONNIE
Status and requires an idle failed state with a current turn/cursor. Unreachable
owners do not imply failure or permit guessed ownership. Fresh Retry remains the
fallback *before* resume is reserved, or after an explicit host rejection.

## Installed APIs and host adapter

Verified against BONNIE **v0.22.1** source in the installed module:

- client/nats exports Status, Cancel/CancelTurn, Submit, Answer; it has no failed-run
  Resume method. Answer requires the trusted waiting suspension.
- runtime.Runner.Resume only accepts RunWaiting. Runner.StartRetry acquires the
  existing run and restores its journal, while respecting explicit cancellation.
- JAWA's reportbridge therefore exposes its own authenticated-by-NATS control route
  `<root>.resume_failed.<agent>`, implemented with StartRetry on the existing RunID,
  empty Text, and the last RecordContext payload. This is a JAWA protocol, not an
  invented BONNIE method. It uses the same pinned admission-index adapter already
  used by typed reports. A future BONNIE upgrade must revalidate that seam.

The host validates exact admission, inactive RunFailed, expected journal cursor and
turn; terminal report intents fail closed. It journals the command before starting
and its result afterward. Duplicate identities return the saved receipt without
running the model again, including after host restart. The runtime keeps its saved
sandbox binding and conversation. Restored context is supplied without duplicating
the original task input.

Migration 8 adds a durable orchestrator command/reservation ledger. Reservation and
active-state transition commit together before dispatch. The dispatcher retries the
same command ID after transport timeouts/restart. Pending commands exclude competing
resume, fresh Retry, reset/delete and cancellation. Old natural outcomes have no
journal cursor; pending deliveries and exact pre-resume outcomes are ignored. The
trusted new receipt supplies the resumed outcome. Old status cursors cannot revive
or undo the resumed generation. Typed reports and independent PR verification still
win; natural completion is not success. Planning can continue into clarify/review
and its existing same-run Answer workflow.

## Operational limits

Deploy updated agents and orchestrator together, and permit the new route in agent
NATS ACLs. Old agents do not answer it. Same stable agent identity and journal are
required. Presence is not a lease: retain the existing single owner per agent/run
and single orchestrator per database/NATS scope assumptions.

If a host crashes after command intent but before its receipt, the adapter does
**not** replay StartRetry. Locally in-flight or runner-active commands remain
pending. An idle orphan intent is finalized with a durable `ambiguous` receipt;
the orchestrator restores the previous failure and stops automatic retries for
that failure generation. Receipt replay and persistence use bounded retries and
log failures. A completed receipt must carry a valid journal cursor.

Pending build/fix and planning badges show **Resuming…**, not Running.
`JAWA_RESUME_STALE_AFTER` configures the reconciliation threshold as a positive Go
duration (default `5m`, for example `30s` or `10m`); invalid values fail workflow
startup. This is not an execution deadline: active and unreachable owners remain
pending. After the threshold, the orchestrator queries the exact owning agent.
For a confirmed idle failed run it sends the same command with `reconcile: true`.
The updated host durably fences unseen commands without executing them, or
supersedes an invalid completed receipt with an ambiguous receipt. Only that host
receipt releases the reservation; elapsed time or Status alone never does. This
prevents a delayed original request from starting after fresh Retry is enabled.
Invalid receipts and stale reservations are logged without result payloads.

Deploy updated agents and orchestrator together: old agents do not implement the
reconciliation fence. There is no admin reconciliation command in this change.
Never delete the ledger to force execution while ownership is unknown.
Missing/expired sandbox storage cannot be rebuilt by this action. External side
effects are not rolled back or made exactly-once by same-run continuation.

Tests use real KIT, BONNIE SQLite journals and local JetStream with an exhausted
scripted provider followed by a healthy provider after restart. They cover both
modes, original prompt/context and identity preservation, concurrent action
reservation, stale forms/events/outcomes, host restart/dedup, missing host, explicit
rejection/fresh fallback, narrow eligibility and crash-window fail-closed behavior.
They make no paid provider calls and do not exercise real remote repositories.

## Automatic runtime recovery

**Auto-resume failed runs** in Settings is enabled by default, including after an
upgrade. It covers planning, Building and Fix runtime failures using the same
ownership and journal guards as manual resume. Existing eligible failures are
scheduled on discovery, not resumed immediately. Delays for automatic commands
1–5 are **5, 10, 20, 40 and 80 seconds**, starting at durable discovery of each
failure generation. Status confirmation failures defer checks by at least five
seconds without consuming a slot. The dispatch pass claims at most 25 due tasks.

The budget is per task/run: manual resumes and planning Answer do not reset or
consume it; fresh Retry creates a new task with a new budget. A committed automatic
reservation consumes a slot even if the host rejects it. Retransmission uses the
same command ID and does not consume another. Rejection stops automation for that
failure generation; manual recovery remains available when safe. After five slots,
operator action is required. Fix preserves its repair branch, PR and message.

Disabling applies immediately to new reservations, but already-reserved commands
continue delivery. Deadlines and budgets survive disable and restart; re-enabling
may claim overdue schedules. Resetting dashboard overrides restores enabled
behavior. Board cards and planning details display deadlines, exhaustion or host
rejection without hiding the original failure. Automation never treats absence,
transport failure, typed blockers, waiting, cancellation or interruption as a
runtime failure. Durable ambiguous receipts stop automation and restore the failed
state; they do not authorize automatic re-execution.

Default-on continuation can incur provider cost and repeat external side effects;
the cap limits additional execution but does not roll back side effects. Wall-clock
changes can affect deadlines. The original agent, journal, sandbox and compatible
control route are still required; upgrade agents with the orchestrator.
