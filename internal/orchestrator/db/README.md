# Database migrations and generated queries

`OpenStore` runs `db.Migrate` before returning a usable store. Workflow schema is
now initialized there too; `migrateWorkflow` remains a compatibility no-op.
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
go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate
```

`sqlc.yaml` reads `schema.sql`, a latest-schema snapshot, because legacy column
adoption and the safe cards rebuild need Go migration logic. Update this snapshot
alongside each new migration. Put queries in `queries/*.sql`, regenerate, and
commit the generated `sqlc/` files. sqlc is a pinned build-time tool, not a runtime
dependency. Queries accept a context; `sqlc.New(tx)` or `queries.WithTx(tx)` binds
queries to a caller's transaction. Generated position/timestamp fields are int64.

The initial slice includes card lookup/list/insert, lane IDs and updates, next
lane position, and project lookup. `Store.Cards` uses the generated list query;
`store_cards_sqlc.go` adapts generated rows to domain cards. Other existing Store
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
