# Typed report bridge contract (tool-side owner)

Actual inspected APIs: BONNIE v0.22.0 `WithKitSetup(func(context.Context,*kit.Kit,bonnie.RunScope) error)` supplies trusted RunID and Session. KIT v0.126.0 `kit.NewTool[T]` handlers receive context with ONLY ToolCallIDFromContext (not task/run/attempt). `k.AddTools` replaces discovered tools by name. We will bind per-run tools in WithKitSetup, not accept identity params. Channel factory captures runtime.Runner. Trusted task/attempt lookup replays BONNIE NATS admission journal `nats_task_admitted` for configured agent, matching runtime RunID; runtime Snapshot provides TurnID. Admission is durable before agent runs.

Shared package I own: internal/reportprotocol. API planned now:
- Kind string constants ProposeSpec="propose_spec", CompleteTask="complete_task", ReportBlocker="report_blocker".
- Identity {TaskID,AgentID,RunID,AttemptID,TurnID string} JSON snake_case.
- Payload {Spec string, Questions []string, RepoCommit string, PRNumber int, Reason string} optional JSON snake_case.
- Request {Version int, ReportID string, Identity embedded, Kind Kind, Payload Payload}.
- Ack {Version int, ReportID string, Accepted bool, Duplicate bool, Error string}.
- Subject(root) => root+".reports.submit" (literal). ACK via Core NATS request/reply ONLY AFTER DB transaction commit. No JetStream publish ACK mistaken for DB commit.
- NewRequest(identity,kind,payload) computes stable SHA256(task_id + kind + canonical normalized payload) ReportID; excludes tool call / turn / attempt so exact same task payload retries stable. Backend must independently validate hash and payload (`Request.Validate()` provided), match exact owner agent/run/attempt and turn as appropriate, reject wrong mode by DB task lookup, reject stale/cancelled/unknown task. One terminal typed report per task; conflicting report payloads rejected. Duplicate ack only for same ID AND exact owner, even after task becomes terminal.

Durability: request/reply consumer commits report inbox + resulting planning/build transition in same DB transaction, unique task and report ID, then sends Ack. Lost replies safely retry; durable report ledger is authoritative. Agent reports only success after accepted matching Ack; transport failure leaves tool retryable (no Halt). Accepted reports Halt agent with typed FinalValue; text outcomes are NOT authoritative. Tool retries use identical report ID, backend exact owner checks still apply. No stream provisioning needed for reports: durability lives in DB ledger plus BONNIE durable tool-call journal/replay. Tool execution interruption may rerun call; tool output lost after commit is safe.

Backend agent owns internal/orchestrator and should implement submit handler + DB ledger and remove parser-driven transitions. Please build against above schema immediately. I will update this file if API changes. Tools model-visible names exactly proposeSpec(spec,questions,repo_commit), completeTask(pr_number), reportBlocker(reason).

## Implemented tool-side API (ready now)

All schema/types above now exist and compile. `reportprotocol.Version=1`,
`NewRequest(...) (Request,error)`, `Request.Validate() error`,
`Client{Conn:*nats.Conn,Root:string}.Submit(ctx,request) (Ack,error)`.
Canonical payload required; NewRequest trims text/SHA and question strings.
Model-facing registration code lives tools/{proposeSpec,completeTask,reportBlocker};
managed runtime adapter is reusable `internal/reportbridge.Bridge` with
`Setup` (bonnie.WithKitSetup) and `Channel(cfg)` (bonnie.Option). main uses it.
Runtime lookup is proven by TestActualNATSAgentReports with actual BONNIE
NATS Channel + real KIT + scripted model for ALL THREE tools; task/run/attempt/
turn identity comes from admission journal and Snapshot, never prompt or args.
Accepted tools Halt; one model step observed. Tests pass. Adapter also forwards
AgentIdentity/PresenceEndpoints so presence remains correct.

IMPORTANT for backend: subscribe `reportprotocol.Subject("bonnie")` using the
same trusted connection as Workflow and reply Ack only after DB commit.
Current client retries 3x at 5s timeout, stable same bytes; receiver rejections
return IsError to model. reportBlocker allowed both planning and build.

Installed `bonnie` executable is OLD and generated incompatible code. Regenerate
with pinned module CLI: `go run github.com/mark3labs/bonnie/cmd/bonnie@v0.22.0 build . --output /tmp/jawa-typed-report-build` (succeeded). No go.mod changes.

Factory-smoke main host now uses shared reportbridge and scripted actual
completeTask tool call instead of legacy JSON text. Waiting on backend handler
before full browser smoke can pass. Lifecycle test fixture will need report
ACK handler because its fake task has no orchestrator; I'll update that locally.

Verification update: all `go test ./...` passed after tool wiring. Actual host
lifecycle smoke also passed with completeTask calls: process shutdown/resume
retains exact task/run/remote-attempt; broker disconnect/reconnect retains them.
Ran `JAWA_LIFECYCLE_SMOKE=1 JAWA_LIFECYCLE_RECONNECT_SMOKE=1 go test ./cmd/factory-smoke -v -count=1` (65s). Those lifecycle fixtures use a validation ACK subscriber only, NOT a database commit claim.

## Durability strengthening (latest)

SDK step journal is written AFTER tool execution, so the adapter now writes a
private durable `jawa_typed_report_intent_v1` Request record BEFORE sending NATS,
and a private `jawa_typed_report_receipt_v1` Ack AFTER receiving DB ACK (including
definite rejections). `Bridge.Setup` replays any unconfirmed intent on restart
BEFORE model prompting, using the ORIGINAL exact request/turn identity. This
closes commit-before-tool-output crash loss without trusting model retries.
Backend duplicates must accept the original trusted turn even if runtime has
since started a recovery turn; dedup should validate the stored exact owner
including original turn BEFORE current-state checks. New report submissions
still require current ownership/mode and active/current task scope.

Race/negative checks passed: `go test -race ./internal/reportbridge
./internal/reportprotocol ./internal/reporttools ./`; `go vet` owned packages;
pinned CLI rebuild. Tests verify unknown run fails closed, spoof identity fields
in model args cannot change trusted identity, unbound discovery tools reject,
and restart replays original durable intent without changing turn or hash.

Backend status observed at 22:18: no typed report handler files yet visible.
Tools are ready; please ensure Workflow installs submit subscriber before agent
execution and use protocol exact field names. Full browser smoke should be run
once handler exists; current factory-smoke emits completeTask, not JSON text.

## FULL BROWSER SMOKE BLOCKER (22:18)

Ran `go run ./cmd/factory-smoke` with real production orchestrator Command,
private broker, authenticated real BONNIE host, real KIT scripted completeTask,
and Playwright. FAILED expected transition timeout: no backend `.reports.submit`
subscriber yet, so tool correctly does not claim completion. Agent identity,
run, attempt are admitted and running; board remains Building. No legacy parser
fallback added. Backend must finish commit/ACK handler then rerun command.

The agent now serializes report intent admission and refuses a DIFFERENT payload
when another is committed or unconfirmed; exact retries reuse original request
including original turn. Backend remains final authority for one task report.

## Tool-side final handoff

Owned implementation complete; no commits. Files: tools/*/tool.go,
main.go + typed_reports.go, regenerated bonnie_gen.go, instructions.md,
internal/reportprotocol (wire validation/hash/ACK client + tests),
internal/reporttools (typed tools + spoof/fail-closed tests),
internal/reportbridge (trusted runtime binding, lifecycle/auth, durable intent
and receipt + real-agent/recovery tests), internal/testmodel (offline scripted
provider), cmd/factory-smoke (actual typed call + lifecycle fixtures).

Final checks: `go test ./...` PASS; owned `go vet` PASS; owned race tests PASS;
pinned BONNIE rebuild PASS; `git diff --check` PASS. Actual lifecycle+reconnect
smokes PASS. Full browser smoke FAILED because production backend report
subscriber/DB commit handler is not implemented yet in visible tree. Do NOT
claim end-to-end DB typed reporting complete until backend implementation and
rerun pass. I did not edit internal/orchestrator owned by backend agent.

Remaining coupling: BONNIE v0.22 does not export admission lookup. Adapter
replicates only documented private status journal cache-key encoding (including
long-key SHA256), tested against real Channel admissions. Upgrade requires
rerunning those tests. Deployment trust boundary remains trusted agents/tools:
NATS shared agent credentials and local sandbox access do not provide hostile
agent isolation; backend must validate exact DB owner, not mere claimed fields.

## Backend completed and end-to-end verified

Backend now installs `bonnie.reports.submit` before dispatch, using the trusted
Workflow connection. Migration 6 creates a durable unique task/report receipt
ledger; report receipt plus planning/build transition commit in one transaction
before Accepted ACK. Exact replay checks original stored agent/run/attempt/turn
before runtime/current-state checks, including after database reopen, task
supersession and card deletion. New submissions validate canonical protocol
payload/hash, task mode, current ownership and active exact BONNIE NATS Status
observation (the pinned v0.22 API exposes Status, not a client Observe method).
Unknown/stale/cancelled/superseded/conflicting submissions reject.

Only typed reports authorize proposals, PR reports or blockers. Natural completed
outcomes without a report fail clearly; accepted typed receipts cannot be
replaced by natural outcomes, events, resync or reconciliation. Planning text
parsers and parser tests are removed entirely. Structured revisions persist
spec/questions/commit; discussion Markdown is built from the accepted fields,
never decoded from assistant text. completeTask still queues the unchanged
independent provider verification gate.

Final backend checks PASS: `go test ./...`, `go vet ./internal/orchestrator/...`,
`go test -race ./internal/orchestrator -count=1`, `git diff --check`, and actual
`go run ./cmd/factory-smoke`. The production offline browser smoke runs real
BONNIE/KIT completeTask calls, receives backend DB-commit ACKs, and verifies
provider blocking, retry/history, restart/login, presence and UI flows. No paid
model calls or live repository/provider writes are claimed. No commits made.
