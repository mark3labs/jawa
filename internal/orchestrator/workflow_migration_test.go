package orchestrator

import "testing"

func TestWorkflowAgentColumnMigration(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context()}
	card := workflowCard(t, s)
	if err := w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE workflow_attempts SET agent_id='existing-agent' WHERE card_id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	for _, column := range []struct{ table, old, name string }{
		{"workflow_attempts", "worker_id", "agent_id"},
		{"workflow_conflicts", "worker_id", "agent_id"},
		{"workflow_execution_events", "worker_id", "agent_id"},
		{"workflow_reconciliations", "previous_worker_id", "previous_agent_id"},
		{"workflow_reconciliations", "selected_worker_id", "selected_agent_id"},
	} {
		if _, err := s.db.Exec(`ALTER TABLE ` + column.table + ` RENAME COLUMN ` + column.name + ` TO ` + column.old); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := migrateWorkflow(s); err != nil {
			t.Fatal(err)
		}
	}
	a, err := w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.AgentID != "existing-agent" {
		t.Fatalf("lost execution identity: %+v", a)
	}
}
