package db

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"jawa/internal/orchestrator/db/sqlc"
	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

func execTest(t *testing.T, database *sql.DB, statement string) {
	t.Helper()
	if _, err := database.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyHistoryPreserved(t *testing.T) {
	for _, oldest := range []bool{true, false} {
		t.Run(map[bool]string{true: "oldest", false: "workflow-era"}[oldest], func(t *testing.T) {
			database := openTestDB(t, filepath.Join(t.TempDir(), "legacy.db"))
			if oldest {
				execTest(t, database, `CREATE TABLE admin(id INTEGER PRIMARY KEY CHECK(id=1),password_hash TEXT NOT NULL);
     CREATE TABLE sessions(token_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
     CREATE TABLE projects(id TEXT PRIMARY KEY,name TEXT NOT NULL,created_at INTEGER NOT NULL);
     CREATE TABLE cards(id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
      title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('Todo','Building','Done')),
      position INTEGER NOT NULL CHECK(position>=0), created_at INTEGER NOT NULL);
     CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);`)
			} else {
				raw, _ := fs.ReadFile(migrations, "migrations/00001_baseline.sql")
				execTest(t, database, string(raw))
			}
			workflow, _ := fs.ReadFile(migrations, "migrations/00003_workflow.sql")
			execTest(t, database, string(workflow))
			execTest(t, database, `PRAGMA foreign_keys=ON;
    INSERT INTO admin(id,password_hash) VALUES(1,'hash');
    INSERT INTO sessions VALUES('token',123);
    INSERT INTO settings VALUES('nats_password','secret-hash');
    INSERT INTO projects(id,name,created_at) VALUES('p','project',10);
    INSERT INTO cards(id,project_id,title,description,status,position,created_at) VALUES('c','p','title','description','Building',7,11);
    INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) VALUES('a','c',1,'task','{}',12,13);
    INSERT INTO workflow_conflicts VALUES(5,'task','agent','run','remote','{}',14,'reason');
    INSERT INTO workflow_execution_events VALUES('task','event','agent','run','remote','started','{}',15);
    INSERT INTO workflow_reconciliations VALUES(6,'task','old-agent','old-run','old-remote','agent','run','remote',5,16);
    CREATE INDEX custom_cards_title ON cards(title);
    CREATE TRIGGER custom_cards_update AFTER UPDATE OF title ON cards BEGIN INSERT OR REPLACE INTO settings VALUES('last_title',NEW.title); END;`)
			// Simulate another feature's history referring to cards before the rebuild.
			execTest(t, database, `CREATE TABLE historical_card_notes(id TEXT PRIMARY KEY,card_id TEXT REFERENCES cards(id) ON DELETE CASCADE);
    INSERT INTO historical_card_notes VALUES('note','c');`)
			ctx := context.Background()
			if err := Migrate(ctx, database); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(ctx, database); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"admin", "sessions", "settings", "projects", "cards", "workflow_attempts", "workflow_conflicts", "workflow_execution_events", "workflow_reconciliations", "historical_card_notes"} {
				var count int
				if err := database.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 1 {
					t.Fatalf("%s count=%d err=%v", table, count, err)
				}
			}
			var enabled int
			if err := database.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
				t.Fatalf("foreign keys=%d err=%v", enabled, err)
			}
			card, err := sqlc.New(database).GetCard(ctx, "c")
			if err != nil || card.Title != "title" || card.Description != "description" || card.Status != "Building" || card.Position != 7 || card.CreatedAt != 11 {
				t.Fatalf("card=%+v err=%v", card, err)
			}
			var hash, username string
			if err := database.QueryRow(`SELECT password_hash,username FROM admin`).Scan(&hash, &username); err != nil || hash != "hash" || username != "admin" {
				t.Fatalf("credentials %s %s: %v", hash, username, err)
			}
			execTest(t, database, `UPDATE cards SET status='Planning',title='updated' WHERE id='c'`)
			execTest(t, database, `UPDATE cards SET status='Fix' WHERE id='c'`)
			var branch, message, pr string
			var source int
			if err := database.QueryRow(`SELECT branch,repair_message,repair_pr_url,repair_source_number FROM workflow_attempts WHERE id='a'`).Scan(&branch, &message, &pr, &source); err != nil || branch != "" || message != "" || pr != "" || source != 0 {
				t.Fatalf("legacy repair defaults: %q %q %q %d %v", branch, message, pr, source, err)
			}
			var title string
			if err := database.QueryRow(`SELECT value FROM settings WHERE key='last_title'`).Scan(&title); err != nil || title != "updated" {
				t.Fatalf("trigger: %s %v", title, err)
			}
			var indexCount int
			if err := database.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='custom_cards_title'`).Scan(&indexCount); err != nil || indexCount != 1 {
				t.Fatalf("index lost: %v", err)
			}
			if _, err := database.Exec(`UPDATE cards SET status='invalid'`); err == nil {
				t.Fatal("invalid status accepted")
			}
			if _, err := database.Exec(`UPDATE cards SET position=-1`); err == nil {
				t.Fatal("negative position accepted")
			}
			if _, err := database.Exec(`UPDATE cards SET project_id='missing'`); err == nil {
				t.Fatal("foreign key enforcement lost")
			}
			execTest(t, database, `DELETE FROM projects WHERE id='p'`)
			for _, table := range []string{"cards", "workflow_attempts", "workflow_conflicts", "workflow_execution_events", "workflow_reconciliations", "historical_card_notes"} {
				var count int
				if err := database.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("cascade %s count=%d err=%v", table, count, err)
				}
			}
		})
	}
}

func TestRebuildFailureRollsBackAndRestoresFK(t *testing.T) {
	database := openTestDB(t, ":memory:")
	baseline, _ := fs.ReadFile(migrations, "migrations/00001_baseline.sql")
	execTest(t, database, string(baseline))
	execTest(t, database, `INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('orphan','missing','title','Todo',0,1)`)
	err := Migrate(context.Background(), database)
	if err == nil || !strings.Contains(err.Error(), "foreign key violation") {
		t.Fatalf("expected validation error, got %v", err)
	}
	var enabled, count int
	if err := database.QueryRow(`PRAGMA foreign_keys`).Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("FK restoration: %d %v", enabled, err)
	}
	if err := database.QueryRow(`SELECT count(*) FROM cards WHERE id='orphan'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("data lost: %d %v", count, err)
	}
	if err := database.QueryRow(`SELECT count(*) FROM goose_db_version WHERE version_id=4 AND is_applied=1`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed migration recorded: %d %v", count, err)
	}
	execTest(t, database, `INSERT INTO projects(id,name,created_at) VALUES('missing','fixed',1)`)
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
}

func TestFreshDatabaseAndGeneratedQueries(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `INSERT INTO projects(id,name,created_at) VALUES('p','project',1)`)
	q := sqlc.New(database)
	for i, status := range []string{"Done", "Building", "Planning", "Todo"} {
		if err := q.InsertCard(ctx, sqlc.InsertCardParams{AutoMergeMode: "inherit", ID: status, ProjectID: "p", Title: status, Status: status, Position: int64(i), CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := q.ListCards(ctx, "p")
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	for i, status := range []string{"Todo", "Planning", "Building", "Done"} {
		if rows[i].Status != status {
			t.Fatalf("wrong order: %v", rows)
		}
	}
	empty, err := q.ListCards(ctx, "missing")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty=%v err=%v", empty, err)
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.WithTx(tx).UpdateCardLane(ctx, sqlc.UpdateCardLaneParams{Status: "Planning", Position: 0, ID: "Todo", ProjectID: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	card, err := q.GetCard(ctx, "Todo")
	if err != nil || card.Status != "Todo" {
		t.Fatalf("transaction binding: %+v %v", card, err)
	}
}

func TestVersionSevenPreservesReceiptsAndInteractionConstraints(t *testing.T) {
	database := openTestDB(t, ":memory:")
	for _, name := range []string{"00001_baseline.sql", "00003_workflow.sql", "00005_planning.sql"} {
		raw, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		execTest(t, database, strings.Split(string(raw), "-- +goose Down")[0])
	}
	execTest(t, database, `INSERT INTO projects(id,name,created_at) VALUES('p','project',1); INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Todo',0,1); INSERT INTO planning_sessions(id,card_id,state,repo,base_branch,provider,title,description,issue_url,spec,revision,created_at,updated_at) VALUES('session','c','questions','repo','main','github','title','','','legacy spec',1,1,1)`)
	legacy, _ := fs.ReadFile(migrations, "migrations/00006_typed_reports.sql")
	// Apply only the legacy Up DDL to simulate existing receipt history.
	execTest(t, database, strings.Split(string(legacy), "-- +goose Down")[0])
	execTest(t, database, `CREATE TABLE goose_db_version(id INTEGER PRIMARY KEY,version_id INTEGER NOT NULL,is_applied INTEGER NOT NULL,tstamp TIMESTAMP DEFAULT CURRENT_TIMESTAMP); INSERT INTO goose_db_version(version_id,is_applied) VALUES(0,1),(1,1),(2,1),(3,1),(4,1),(5,1),(6,1)`)
	execTest(t, database, `INSERT INTO typed_reports VALUES('task','receipt','agent','run','attempt','turn','complete_task','original JSON',123)`)
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	var state, spec string
	if err := database.QueryRow(`SELECT state,spec FROM planning_sessions WHERE id='session'`).Scan(&state, &spec); err != nil || state != "failed" || spec != "legacy spec" {
		t.Fatal(state, spec, err)
	}
	var raw, call string
	if err := database.QueryRow(`SELECT request_json,tool_call_id FROM typed_reports WHERE report_id='receipt'`).Scan(&raw, &call); err != nil || raw != "original JSON" || call != "legacy:receipt" {
		t.Fatal(raw, call, err)
	}
	if _, err := database.Exec(`INSERT INTO typed_reports VALUES('other','task','other-call','agent','run','attempt','turn','complete_task','{}',124)`); err == nil {
		t.Fatal("second completion accepted")
	}
	// Blockers awaiting guidance park the run, so a task may report several.
	execTest(t, database, `INSERT INTO typed_reports VALUES('blocker','task','blocker-call','agent','run','attempt','turn','report_blocker','{}',124)`)
	execTest(t, database, `INSERT INTO typed_reports VALUES('interaction','task','new-call','agent','run','attempt','turn','clarify','{}',124)`)
	if _, err := database.Exec(`INSERT INTO typed_reports VALUES('conflict','task','new-call','agent','run','attempt','turn','clarify','{}',125)`); err == nil {
		t.Fatal("same tool identity accepted twice")
	}
	var version int
	if err := database.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil || version != 24 {
		t.Fatal(version, err)
	}
}

func TestVersionTwentyThreeCreatesAPIKeyTables(t *testing.T) {
	database := openTestDB(t, ":memory:")
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"api_keys", "mcp_requests"} {
		var name string
		if err := database.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatal(err)
		}
	}
	execTest(t, database, `INSERT INTO api_keys(id,name,token_hash,prefix,created_at) VALUES('a','client','hash','prefix',1)`)
	if _, err := database.Exec(`INSERT INTO api_keys(id,name,token_hash,prefix,created_at) VALUES('b',?,'other','prefix',1)`, strings.Repeat("x", 65)); err == nil {
		t.Fatal("long name accepted")
	}
	if _, err := database.Exec(`INSERT INTO api_keys(id,name,token_hash,prefix,created_at) VALUES('b','client','hash','prefix',1)`); err == nil {
		t.Fatal("duplicate hash accepted")
	}
}

func TestPRCheckRefreshMigrationPreservesEvidence(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Done',0,1);
 INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) VALUES('a','c',1,'task','{}',1,1);
 DROP TABLE workflow_pr_checks`)
	legacy, err := fs.ReadFile(migrations, "migrations/00010_pr_checks.sql")
	if err != nil {
		t.Fatal(err)
	}
	execTest(t, database, strings.Split(string(legacy), "-- +goose Down")[0])
	execTest(t, database, `INSERT INTO workflow_pr_checks VALUES('a','url','github','repo','main','conflict','head','',123);
 ALTER TABLE cards DROP COLUMN archived;
 DROP TABLE auto_resumes;
 ALTER TABLE failed_resumes DROP COLUMN automatic;
 DROP TABLE workflow_ready_overrides;
 DROP TABLE workflow_pr_merge_overrides;
 DROP TABLE workflow_interactions;
 DROP INDEX typed_reports_terminal;
 CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind IN ('complete_task','report_blocker');
 DROP TABLE pr_merges; DROP TABLE issue_submissions; ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,11,12,13,14,15,16,17,18,19,20,21)`)
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	var state, head, base string
	var checked int64
	if err := database.QueryRow(`SELECT state,head_sha,base_sha,checked_at FROM workflow_pr_checks WHERE attempt_id='a'`).Scan(&state, &head, &base, &checked); err != nil || state != "conflict" || head != "head" || base != "" || checked != 123 {
		t.Fatalf("%s %s %s %d %v", state, head, base, checked, err)
	}
	execTest(t, database, `UPDATE workflow_pr_checks SET state='checking',base_sha='base',checked_at=0`)
	if _, err := database.Exec(`UPDATE workflow_pr_checks SET state='invalid'`); err == nil {
		t.Fatal("invalid state accepted")
	}
	execTest(t, database, `DELETE FROM cards WHERE id='c'`)
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM workflow_pr_checks`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade %d %v", count, err)
	}
}

func TestAutoResumeMigrationDefaultsAndCascade(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TABLE auto_resumes; ALTER TABLE failed_resumes DROP COLUMN automatic;
 DROP TABLE workflow_ready_overrides;
 DROP TABLE workflow_pr_merge_overrides;
 DROP TABLE workflow_interactions;
 DROP INDEX typed_reports_terminal;
 CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind IN ('complete_task','report_blocker');
 DROP TABLE pr_merges; DROP TABLE issue_submissions; ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,13,14,15,16,17,18,19,20,21);
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Building',0,1);
 INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('old','task','c',0,'{}','{}',1);`)
	for range 2 {
		if err := Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	var automatic int
	if err := database.QueryRow(`SELECT automatic FROM failed_resumes WHERE id='old'`).Scan(&automatic); err != nil || automatic != 0 {
		t.Fatal(automatic, err)
	}
	execTest(t, database, `INSERT INTO auto_resumes(task_id,card_id,planning,failure_token,due_at,next_check_at,state) VALUES('task','c',0,'token',123,123,'scheduled')`)
	// Version 24 adds untriaged defaults and constrains the class.
	var triage string
	var permanent float64
	if err := database.QueryRow(`SELECT triage,triage_probability FROM auto_resumes WHERE task_id='task'`).Scan(&triage, &permanent); err != nil || triage != "" || permanent != 0 {
		t.Fatal(triage, permanent, err)
	}
	execTest(t, database, `UPDATE auto_resumes SET triage='permanent',triage_probability=0.9`)
	if _, err := database.Exec(`UPDATE auto_resumes SET triage='maybe'`); err == nil {
		t.Fatal("invalid triage accepted")
	}
	if _, err := database.Exec(`UPDATE failed_resumes SET automatic=2`); err == nil {
		t.Fatal("invalid origin accepted")
	}
	execTest(t, database, `DELETE FROM cards WHERE id='c'`)
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM auto_resumes`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	rows, err := database.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}

func TestBlockerGuidanceMigrationAllowsRepeatedBlockers(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TABLE workflow_ready_overrides;
 DROP TABLE workflow_interactions;
 DROP INDEX typed_reports_terminal;
 CREATE UNIQUE INDEX typed_reports_terminal ON typed_reports(task_id) WHERE kind IN ('complete_task','report_blocker');
 DROP TABLE pr_merges; DROP TABLE issue_submissions; ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; ALTER TABLE auto_resumes DROP COLUMN triage_probability; ALTER TABLE auto_resumes DROP COLUMN triage; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,15,16,17,18,19,20,21);
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Building',0,1);
 INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) VALUES('a','c',1,'task','{}',1,1);
 INSERT INTO typed_reports VALUES('legacy','task','legacy-call','agent','run','attempt','turn','report_blocker','{}',1);`)
	for range 2 {
		if err := Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	execTest(t, database, `INSERT INTO typed_reports VALUES('second','task','second-call','agent','run','attempt','turn','report_blocker','{}',2);
 INSERT INTO workflow_interactions(report_id,task_id,created_at) VALUES('second','task',2);
 INSERT INTO typed_reports VALUES('done','task','done-call','agent','run','attempt','turn','complete_task','{}',3);`)
	if _, err := database.Exec(`INSERT INTO typed_reports VALUES('again','task','again-call','agent','run','attempt','turn','complete_task','{}',4)`); err == nil {
		t.Fatal("second completion accepted")
	}
	execTest(t, database, `INSERT INTO typed_reports VALUES('third','task','third-call','agent','run','attempt','turn','report_blocker','{}',5)`)
	if _, err := database.Exec(`INSERT INTO workflow_interactions(report_id,task_id,created_at) VALUES('third','task',5)`); err == nil {
		t.Fatal("two open blockers accepted for one task")
	}
	execTest(t, database, `DELETE FROM cards WHERE id='c'`)
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM workflow_interactions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cascade %d %v", count, err)
	}
}

func TestFailedOutcomeRunStateMigrationReleasesStuckAttempts(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TABLE pr_merges; DROP TABLE issue_submissions; ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; ALTER TABLE auto_resumes DROP COLUMN triage_probability; ALTER TABLE auto_resumes DROP COLUMN triage; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,16,17,18,19,20,21);
 DROP TABLE workflow_ready_overrides;
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Building',0,1);
 INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,state,run_state,outcome_json,created_at,updated_at) VALUES
  ('stuck','c',1,'stuck','{}','failed','running','{"state":"running","error":"admitted answer resume interrupted"}',1,1),
  ('live','c',2,'live','{}','running','running','',1,1),
  ('quiet','c',3,'quiet','{}','failed','running','{"state":"running"}',1,1),
  ('waiting','c',4,'waiting','{}','blocked','waiting','{"state":"waiting","error":"x"}',1,1);`)
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"stuck": "failed", "live": "running", "quiet": "running", "waiting": "waiting"} {
		var got string
		if err := database.QueryRow(`SELECT run_state FROM workflow_attempts WHERE id=?`, id).Scan(&got); err != nil || got != want {
			t.Fatalf("%s run_state=%q want %q (%v)", id, got, want, err)
		}
	}
}

func TestIssueSubmissionUpgradeAndTombstone(t *testing.T) {
	database := openTestDB(t, ":memory:")
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TABLE pr_merges; DROP TABLE issue_submissions; ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; ALTER TABLE auto_resumes DROP COLUMN triage_probability; ALTER TABLE auto_resumes DROP COLUMN triage; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,18,19,20,21);
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Todo',0,1);`)
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `INSERT INTO issue_submissions(token,project_id,provider,repository,title,body,state,card_id,created_at,updated_at) VALUES('token','p','github','repo','title','body','completed','c',1,1);
 DELETE FROM cards WHERE id='c'; DELETE FROM projects WHERE id='p';`)
	var card string
	if err := database.QueryRow(`SELECT card_id FROM issue_submissions WHERE token='token'`).Scan(&card); err != nil || card != "c" {
		t.Fatalf("tombstone %s %v", card, err)
	}
	if _, err := database.Exec(`UPDATE issue_submissions SET state='retryable'`); err == nil {
		t.Fatal("invalid state accepted")
	}
}

func TestReadyOverrideMigrationFrom16(t *testing.T) {
	database := openTestDB(t, ":memory:")
	ctx := context.Background()
	if err := Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TABLE workflow_ready_overrides; DROP TABLE pr_merges; DROP TABLE issue_submissions;
 ALTER TABLE workflow_attempts DROP COLUMN target_agent_id; ALTER TABLE planning_turns DROP COLUMN target_agent_id; DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations; ALTER TABLE cards DROP COLUMN auto_merge_mode; DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; ALTER TABLE auto_resumes DROP COLUMN triage_probability; ALTER TABLE auto_resumes DROP COLUMN triage; DELETE FROM goose_db_version WHERE version_id IN (24,23,22,17,18,19,20,21);
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Building',0,1);
 INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,outcome_json,created_at,updated_at) VALUES('a','c',1,'task','{}','original',1,1);
 INSERT INTO typed_reports VALUES('report','task','call','agent','run','attempt','turn','complete_task','{}',1);`)
	for range 2 {
		if err := Migrate(ctx, database); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err := database.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil || version != 24 {
		t.Fatal(version, err)
	}
	var original string
	if err := database.QueryRow(`SELECT outcome_json FROM workflow_attempts WHERE id='a'`).Scan(&original); err != nil || original != "original" {
		t.Fatal(original, err)
	}
	var count int
	if err := database.QueryRow(`SELECT count(*) FROM typed_reports`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	execTest(t, database, `PRAGMA foreign_keys=ON;
 INSERT INTO workflow_ready_overrides VALUES('a','pr','sha','token','{}','operator','reason',1);`)
	if _, err := database.Exec(`INSERT INTO workflow_ready_overrides VALUES('a','pr','sha','token','{}','operator','reason',2)`); err == nil {
		t.Fatal("duplicate attempt allowed")
	}
	if _, err := database.Exec(`INSERT INTO workflow_ready_overrides VALUES('missing','pr','sha','token','{}','operator','reason',2)`); err == nil {
		t.Fatal("missing attempt allowed")
	}
	execTest(t, database, `DELETE FROM workflow_attempts WHERE id='a'`)
	if err := database.QueryRow(`SELECT count(*) FROM workflow_ready_overrides`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestTargetMigrationPreservesExecutionHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "assignment.db")
	database := openTestDB(t, path)
	for _, name := range []string{"00001_baseline.sql", "00003_workflow.sql", "00005_planning.sql"} {
		raw, err := fs.ReadFile(migrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		execTest(t, database, strings.Split(string(raw), "-- +goose Down")[0])
	}
	execTest(t, database, `INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','card','Todo',0,1);
 INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,agent_id,run_id,remote_attempt_id,outcome_json,created_at,updated_at) VALUES('task','c',1,'task','immutable','observed','run','remote','history',1,1);
 INSERT INTO planning_sessions(id,card_id,repo,base_branch,provider,title,description,issue_url,created_at,updated_at) VALUES('session','c','repo','main','github','title','','',1,1);
 INSERT INTO planning_turns(id,session_id,number,task_json,agent_id,run_id,remote_attempt_id,created_at,updated_at) VALUES('turn','session',1,'planning payload','planner','planning run','planning remote',1,1);`)
	raw, err := fs.ReadFile(migrations, "migrations/00019_target_agents.sql")
	if err != nil {
		t.Fatal(err)
	}
	execTest(t, database, string(raw))
	var target, agent, run, remote, payload, history string
	if err = database.QueryRow(`SELECT target_agent_id,agent_id,run_id,remote_attempt_id,task_json,outcome_json FROM workflow_attempts`).Scan(&target, &agent, &run, &remote, &payload, &history); err != nil {
		t.Fatal(err)
	}
	if target != "" || agent != "observed" || run != "run" || remote != "remote" || payload != "immutable" || history != "history" {
		t.Fatal("migration changed execution history")
	}
	if err = database.QueryRow(`SELECT target_agent_id,agent_id FROM planning_turns`).Scan(&target, &agent); err != nil || target != "" || agent != "planner" {
		t.Fatalf("planning history %q %q %v", target, agent, err)
	}
	execTest(t, database, `UPDATE workflow_attempts SET target_agent_id='builder'; UPDATE planning_turns SET target_agent_id='planner-target'`)
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestDB(t, path)
	if err = reopened.QueryRow(`SELECT target_agent_id,agent_id FROM workflow_attempts`).Scan(&target, &agent); err != nil || target != "builder" || agent != "observed" {
		t.Fatalf("reopen %q %q %v", target, agent, err)
	}
	if err = reopened.QueryRow(`SELECT target_agent_id FROM planning_turns`).Scan(&target); err != nil || target != "planner-target" {
		t.Fatalf("reopen turn %q %v", target, err)
	}
}

// The base branch already shipped migration 20 for operator merges.
func TestAutoMergeMigrationFromOperatorMergeSchema(t *testing.T) {
	database := openTestDB(t, ":memory:")
	if err := Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	execTest(t, database, `DROP TRIGGER auto_merge_card_update; DROP TRIGGER auto_merge_card_delete; DROP TABLE auto_merge_operations;
 ALTER TABLE cards DROP COLUMN auto_merge_mode; ALTER TABLE issue_submissions DROP COLUMN auto_merge_mode;
 DROP TABLE run_chat; DROP TABLE mcp_requests; DROP TABLE api_keys; ALTER TABLE auto_resumes DROP COLUMN triage_probability; ALTER TABLE auto_resumes DROP COLUMN triage; DELETE FROM goose_db_version WHERE version_id IN (24,23,21,22);
 INSERT INTO projects(id,name,created_at) VALUES('p','project',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','existing','Done',0,1);
 INSERT INTO pr_merges VALUES('op','c','attempt','pr','snapshot','github','acme/repo','main','head','squash','operator','uncertain',1,1);`)
	if err := Migrate(t.Context(), database); err != nil {
		t.Fatal(err)
	}
	var mode, state string
	if err := database.QueryRow(`SELECT auto_merge_mode FROM cards WHERE id='c'`).Scan(&mode); err != nil || mode != "inherit" {
		t.Fatal(mode, err)
	}
	if err := database.QueryRow(`SELECT state FROM pr_merges WHERE id='op'`).Scan(&state); err != nil || state != "uncertain" {
		t.Fatal(state, err)
	}
	execTest(t, database, `PRAGMA foreign_key_check`)
}
