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

Planner protocol: BONNIE v0.22 typed Task/Outcome over existing bonnie JetStream. Each conversation turn is a new deterministic durable task (full immutable repository/card context + conversation + previous spec); terminal response JSON `{ "spec": "...", "questions": ["..."], "repo_commit": "full git SHA" }`. No PR verification. Native waiting is not used for conversation: planner must return questions in a completed outcome. User reply queues another independent task, avoiding non-durable remote resume. Read-only investigation is explicitly scoped in prompt; deployment must give planner read-only repository credentials/tool sandbox (BONNIE wire Task has no tool capability restriction). Persistence and delivery guards protect against stale sessions, duplicate results and mismatched remote identity.

## Implementation / verification update

Backend APIs are implemented and `TestPlanning*` passes, including durable database reopen, actual BONNIE JetStream task publish/result consumption and workflow restart, conversation questions/reply/revision, CAS approval races, cancellation/new-session late result guards, pinned commit rejection, malformed planner output, atomic rollback, and approved-spec preservation on build retries. `go test ./...` passed after UI integration; planning race tests also pass.

Important: revision numbers are **card-global monotonic tokens**, not restarted at 1 per session. This prevents an old approval form from a cancelled session approving a new session with the same revision number. Session-local revision lists can therefore begin at e.g. 3; display Number verbatim.

Cancellation returns the card to Todo immediately and invalidates queued/running planner authority. Published tasks receive best-effort exact-turn BONNIE cancellation via a durable local retry queue; timeout/offline agent does not imply remote stop. Late event admissions are pinned solely for cancellation. All raw planning deliveries (including duplicate/conflicting/late results) are retained in planning_deliveries; no late output may change a cancelled/approved/superseded session. Native BONNIE waiting/interrupted outcomes fail this planner contract and are retried as fresh tasks; UI user conversation uses completed JSON questions, not remote ask-user suspension.

Read-only scope is prompt-level in this integration because BONNIE v0.22 Task exposes only Text/Context and no tool allowlist. Enforce real isolation in the planner agent's credentials/tool configuration; do not mistake the prompt for a security sandbox. Result repo_commit is reported by agent, not independently attested. Sessions pin the first valid full SHA, and subsequent outputs must match it. Retained-stream recovery is bounded to the latest 10,000 messages/5 seconds (same operational limit as existing build workflow); broker retention must cover outages.

Final backend verification: `go test ./...`, `go vet ./internal/orchestrator/...`, `go test -race ./internal/orchestrator -run TestPlanning -count=1`, and `git diff --check` pass. Additional smoke tests cover raw wire task identity/text, retained-stream outcome replay idempotence, exact observed-turn cancellation commands, revision CAS across cancelled/restarted sessions, history FK cascades and lane compaction. Tests use real embedded NATS JetStream and typed BONNIE clients with simulated agent reports; they do not run a live LLM/repository investigation. No commits made. Backend did not edit server/templates/frontend/UI/view files owned by UI agent.

## Planning output parsing

The production pure-Go parser (`planning_output.go`) is shared by outcome
validation and the Markdown report renderer. It accepts bare JSON, a single
unlabelled or case-insensitive `json` backtick fence, and Markdown headings/prose
before or after one unambiguous object. CRLF and a leading UTF-8 BOM are supported.
Fence lines must stand alone; unsupported, unclosed, empty, or multiple fences
are rejected. Structural braces/brackets outside the report are conservative
ambiguity errors, not ignored examples. Quoted prose cannot supply a candidate.

Only narrow syntax repairs are performed: trailing object/array commas, bare
**top-level** `spec`/`questions`/`repo_commit` keys, and JSON-style line/block
comments outside strings. Original string tokens are preserved; malformed escapes,
unescaped quotes/newlines, invalid UTF-8, lone UTF-16 surrogate escapes, truncation,
and speculative string repairs are rejected. Valid Unicode surrogate pairs decode
normally, without replacement characters.
Every object is scanned for duplicate decoded keys (including escaped aliases and
nested objects) before schema decoding. Unknown/case-aliased keys are rejected.
`spec` and `repo_commit` must be strings; `questions` is explicitly required and
must be an array of strings (`[]` is valid, missing/null/scalar/null elements are
not). No numeric, boolean, or scalar coercion is performed.

The entire response is bounded to 256000 bytes before processing, with a maximum
JSON value nesting depth of 32. Existing normalization remains: trim the spec and
questions, trim/lowercase the full 40/64-hex commit SHA, require nonempty spec and
questions, at most 30 questions and at most 4000 original bytes per question.
SHA syntax is not proof the agent inspected that commit; pinned-session checks
remain unchanged. Errors do not include private response content.

Regression coverage includes adversarial code/fake report fields inside spec,
ambiguity, duplicates, source types, every-byte truncation and fuzz round trips.
`TestPlanningOutputSavedResponse` is opt-in via `JAWA_PLANNING_SAVED_RESPONSE`:
it independently decodes the stored single fence, verifies exact extraction, then
checks the existing normalization. Do not store private responses in fixtures;
read the source database with SQLite `mode=ro` and `PRAGMA query_only=ON`.
