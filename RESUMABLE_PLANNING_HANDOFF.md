# Resumable planning tool/API handoff (in progress)

Tool-side owner only; backend implementation separate. Pinned BONNIE v0.22.0 / KIT v0.126.0 inspected.

Contract changes being implemented now:
- `Clarify Kind = "clarify"`; clarify(questions) requires 1..30 nonempty questions. No spec/commit/build fields.
- proposeSpec(spec,repo_commit) has NO questions input; protocol unified Payload retains Questions only for clarify, and rejects them for propose_spec.
- Identity adds trusted `ToolCallID string json:"tool_call_id"`, obtained exclusively from KIT ToolCallIDFromContext. Required. Report hash includes task + tool_call_id + kind + normalized payload. Multiple interactions per same task/run/turn are distinct. Exact replay retains original identity/hash.
- clarify and propose_spec ACKs are durable *interaction* commits, not terminal task completion. Backend ledger must support multiple reports per task (unique report ID and/or task/tool-call ID), exact owner checking includes tool_call_id. complete_task/report_blocker remain terminal.
- Accepted clarify parks with runtime.SuspendRequest{Kind:"question",Prompt:joined questions,ToolCallID:trusted ID,TurnID:trusted turn}. Accepted propose_spec parks with Kind:"approval", Prompt:spec, Options:["approve","reject"], same IDs. Backend must resume via BONNIE Answer on SAME RunID with suspension TurnID (and Text/Approved), not dispatch a new task/run. Rejection/revision text continues original conversation.

Verified actual API: runtime/suspend.go exports SuspendRequest with TurnID, Kind, Prompt, Options, ToolCallID (NO Questions/Input fields). runtime.suspensionFrom accepts ONLY concrete runtime.SuspendRequest or *runtime.SuspendRequest, not arbitrary Request/map. KIT propagates FinalValue dynamic type unchanged. No runtime extension hook is needed to map custom result: host-bound tool output adapter returns actual SuspendRequest directly after DB ACK. WithKitSetup registers these bound custom tools. KIT OnAfterToolResult cannot safely supply FinalValue (inspecting hook); direct binding adapter is the supported path.

Durability: existing report intent-before-send and receipt-after-ACK stays, now per interaction. Original trusted request survives retries. Runtime writes RecordStep suspend payload and RecordSuspend on tool Halt; existing BONNIE journal/restoration handles waiting/restart/Answer. Tool-side tests will verify actual NATS+KIT cycle and restart while waiting. No live DB writes. Protocol version remains 1 for coordinated source deployment; old binaries incompatible with required tool_call_id.

Implementation update: shared protocol now compiled with Clarify + required ToolCallID; tools registered and actual NATS tests pass for question/review waiting states. Backend fixtures constructing Identity must add ToolCallID; legacy propose_spec Questions no longer accepted. Existing protocol hash still excludes turn/attempt (original retries retained), includes task+tool_call_id+kind+payload. Backend must compare complete Identity, not just first five fields.

Correction: BONNIE RecordStep payload is only a generic step marker, NOT the halting SuspendRequest. RecordSuspend is the durable waiting request. There is a crash window after report receipt and before runtime writes RecordSuspend. Bridge startup will repair accepted unresumed planning interactions into runtime RecordSuspend/RunWaiting before channel activation, using original journaled request. It will resolve original tool intent text from trusted receipt on subsequent setup so provider conversation stays valid. No custom runtime-extension API was found; direct bound FinalValue mapping is the actual supported API.

## Tool-side implementation and verification (23:18)

Implemented in internal/reportprotocol, internal/reporttools, internal/reportbridge,
tools/clarify, instructions.md and regenerated bonnie_gen.go (pinned CLI).
Host-bound `Binding.FinalValue` adapter maps committed reports to actual runtime
SuspendRequest; public KIT after-tool hook exposes ONLY string/IsError, not
FinalValue. WithKitSetup is the verified registration seam, not an invented
runtime extension. completeTask/reportBlocker still return terminal Request.

Bridge serializes intent writes, stores per-interaction receipts, permits new
planning interactions after committed planning reports (not after a terminal
report), blocks new work while any intent is unconfirmed, and rejects different
payloads for the same trusted tool-call ID. Startup repairs missing runtime
suspend/wait checkpoint from accepted receipt before channel admission; existing
RecordResume prevents resurfacing consumed interactions. SDK interrupted tool
outcome is resolved from trusted receipt before BONNIE's transcript recovery.

PASS: owned tests, owned+root+factory-smoke race tests, owned vet, pinned CLI
build, diff check. TestPlanningAnswerSameRunAfterRestart uses actual local NATS
JetStream, SQLite BONNIE journal reopened three times, real KIT scripted calls:
clarify -> Answer -> proposeSpec -> reject Answer -> revised proposeSpec ->
approve Answer -> terminal blocker. Checks same TaskID/RunID/AttemptID, waiting
suspension after reopening, distinct report IDs, trusted tool-call IDs and
original conversation + all review/answer text retained. ACK subscriber is a
validation fixture, not a database commit claim. Crash-window fixture verifies
accepted receipt without RecordSuspend is repaired idempotently and consumed
resume is not resurrected. No live DB/provider writes.

Existing `go run ./cmd/factory-smoke` PASS with scripted real completeTask and
production backend DB ACK; terminal complete flow remains compatible. This is
NOT a planning backend end-to-end claim.

Full `go test ./...` currently FAILS in backend fixtures: missing required trusted
tool_call_id, and old propose_spec questions fixtures. Backend agent must update
these alongside interaction ledger/workflow changes. Backend-owned code/tests
not edited by tool-side owner. Required new identities: typed_reports_test.go,
typed_report_helpers_test.go, cancellation_test.go. Protocol Version remains 1
only for coordinated deployment; consider backend bump/migration policy.

Lifecycle/reconnect smokes also PASS (66s):
`JAWA_LIFECYCLE_SMOKE=1 JAWA_LIFECYCLE_RECONNECT_SMOKE=1 go test ./cmd/factory-smoke -v -count=1`.

Operational detail: a pending ambiguous interaction blocks *new* tool-call IDs,
even for equal payloads; restart recovery replays original intent automatically.
The SDK cannot replay typed FinalValue from its text-only tool outcome; the
bridge repairs accepted pending planning suspension on channel startup. On a
transport error operators should recover/restart the host for original-intent
replay rather than manufacture a new tool identity. Backend should provide
idempotent original report ACKs; it must not infer DB success from NATS publish.

Final retry refinement supersedes prior operational note: Binding.Prepare now
coalesces an identical-payload retry ONLY while an original durable intent is
unconfirmed, preserving its original complete Identity/ReportID/turn/call ID.
Changed payloads reject. Once receipt resolves that interaction, identical
later planning calls are new interactions. Restart recovery also restores the
original turn marker if interrupted recovery had begun a newer runtime turn,
so Answer.TurnID can correctly target the restored suspension. Regression test
TestAmbiguousRetryKeepsOriginalInteraction covers this distinction.
