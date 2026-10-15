# Database migrations and generated queries

`OpenStore` runs `db.Migrate` before returning a usable store. Workflow schema is
initialized there too.
Migrations are embedded, so deployment does not need SQL files on disk. Goose's
provider API uses no global registration or dialect state.

## Migration history

- `00001_baseline.sql`: adopts the original tables with `IF NOT EXISTS`.
- Version 2 (Go, `upgradeLegacyColumns`): adds missing legacy admin/project/card
  columns by inspecting the schema; existing values are untouched.
- `00003_workflow.sql`: adopts existing workflow tables, including conflicts,
  execution events and reconciliations.
- Version 4 (Go, `addPlanningStatus`): rebuilds cards with the
  `Todo / Planning / Building / Done` constraint. Explicit indexes and triggers
  are restored. Child FK targets are never renamed. A full foreign-key check
  runs inside the rebuild transaction before the version is recorded.

Use new, increasing versions; do not edit applied migrations. Versions 2 and 4
are Go migrations and intentionally have no SQL files. Downgrades are not
supported (history is retained rather than dropped). Use the application
bootstrap, not standalone Goose CLI, which cannot see these Go migrations.

The bootstrap requires a single persistent SQLite connection and no concurrent
queries during initialization. Foreign-key enforcement is disabled *outside*
Goose's transactions only during startup and restored even on failure. A failed
rebuild rolls back data and its Goose version record. A legacy database with
orphaned foreign keys is rejected rather than silently losing history; repair
those references and reopen. Keep an operational backup before upgrades.

## sqlc

From the repository root:

```sh
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate
```

`sqlc.yaml` reads `schema.sql`, a latest-schema snapshot, because legacy column
adoption and the safe cards rebuild need Go migration logic. Update this snapshot
alongside each new migration. Put queries in `queries/*.sql`, regenerate, and
commit the generated `sqlc/` files. sqlc is a pinned build-time tool, not a runtime
dependency. Queries accept a context; `sqlc.New(tx)` or `queries.WithTx(tx)` binds
queries to a caller's transaction. Generated position/timestamp fields are int64.

The initial slice includes card lookup/list/insert, lane IDs and updates, next
lane position, and project lookup. `Store.Cards` uses the generated list query;
`store_cards_sqlc.go` adapts generated rows to domain cards. Card creation and lane
compaction use transaction-bound generated queries. Other existing Store
mutations are being adopted incrementally. The generated lane update is a
low-level primitive, not a replacement for lane compaction. CI verifies that
regenerating with the pinned sqlc version produces no diff.

## Planning persistence

Planning session creation and the card's transition to Planning share the
existing write transaction. Add future tables through migrations and update
the schema snapshot; do not introduce a second schema initializer or edit
applied migrations.

Version `00005_planning.sql` now creates durable planning sessions, messages,
immutable revisions, queued turns, and raw delivery history. Planning reads and
queue selection use generated sqlc queries; atomic business mutations bind SQL
and generated queries to Store.writeTx. Revision tokens increase across all
sessions for a card (even after cancellation), protecting old approval forms.
`cancel_sent` tracks acknowledged best-effort BONNIE exact-turn cancellation;
local cancellation invalidates authority independently of agent availability.

## Version 7 resumable interactions

`00007_resumable_planning.sql` retains receipt JSON/owner history while changing
receipt uniqueness to report ID and task/tool-call ID. Terminal reports remain
one-per-task. `planning_interactions` is the durable reply outbox; only one
pending/queued interaction per task is allowed. Waiting outcome and immutable
response must be persisted before dispatch. sqlc exposes interaction history and
queued-reply selection. Legacy ready/questions sessions cannot be resumed because
v6 completed their remote run: preserve history/spec but mark them failed with an
explicit retry message. Deploy upgraded agents/backend together.

Version `00008_failed_resume.sql` adds the failed-run command reservation ledger,
retaining original outcomes and durable request/reply audit without creating new
execution identities. See `../FAILED_RESUME.md` for restart and ambiguity semantics.

Version `00013_auto_resume.sql` adds the `automatic` ledger discriminator (existing
commands default to manual) and `auto_resumes` scheduling hints. Deadlines and
failure tokens persist across restart; budgets are derived from automatic command
reservations, never a separate counter. Card deletion cascades scheduling hints.
The default-enabled setting requires no data rewrite; absent overrides mean true.
See `../FAILED_RESUME.md` for budget, rejection and disable semantics.

Version `00015_blocker_guidance.sql` narrows the one-per-task terminal index to
`complete_task`, because a blocker awaiting guidance only parks its run and a task
may block more than once. It adds `workflow_interactions`, the durable build
guidance outbox (one pending/queued blocker per task, cascading with attempts).
Existing blockers have no interaction row and remain terminal. See
`../BLOCKER_GUIDANCE.md`.

Version `00018_issue_submissions.sql` adds the New card issue-write reservation
ledger. No credentials or raw provider responses are stored. Draft, configured
repository/provider, minimal outcome and completion evidence survive card/project
deletion intentionally, so a replay cannot recreate a deleted resource. Reserved
writes become uncertain on application startup; this is not a background outbox
and never triggers remote retries. Use one orchestrator per database.

Version `00019_target_agents.sql` adds desired `target_agent_id` to workflow
attempts and planning turns. Empty defaults keep historical work shared without
changing observed execution identity or history. Targets persist across publish
retries/restart and are copied to fresh Retry tasks; they are not run ownership.

Version `00021_auto_merge.sql` adds constrained live card/issue-draft mode
(default inherit) and a non-cascading merge-operation ledger. Tombstones survive
card/project deletion; no credentials/raw responses are persisted. Reserved/inflight
rows fence card lane/delete/archive mutations and become uncertain on startup.
Settings reset preserves card modes; absent global dashboard:auto-merge is false.
Back up databases before migration and run only one orchestrator per database.
Version `00020_pr_merges.sql` adds the durable operator merge audit and partial
unique unresolved-card index. Audit rows intentionally have no cascading foreign
keys so deletion cannot erase replay evidence. They retain operation/card/attempt,
PR/configuration snapshot, expected head, method, operator, timestamps and safe
outcome only. Pending reservations become uncertain at startup; reconciliation
never replays a write. Back up before upgrading. Snapshot and pinned sqlc output
must accompany this migration.
