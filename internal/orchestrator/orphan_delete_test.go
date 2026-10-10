package orchestrator

import "testing"

func TestDeleteUnreconciledRequiresAcknowledgment(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteUnreconciledCard(c.ID); err == nil {
		t.Fatal("ordinary active work force deleted")
	}
	if _, err := s.db.Exec(`UPDATE workflow_attempts SET state='running',run_state='running',error=? WHERE card_id=?`, executionConflictNotice, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.DeleteCard(c.ID); err == nil {
		t.Fatal("unacknowledged deletion allowed")
	}
	if err := w.ResetCard(c.ID); err == nil {
		t.Fatal("reset orphaned execution")
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !canAction(Card{ID: c.ID, Status: "Building"}, []Attempt{a}, "delete") {
		t.Fatal("delete hidden")
	}
	if err := w.DeleteUnreconciledCard(c.ID); err != nil {
		t.Fatal(err)
	}
	cards, err := s.Cards("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatal("card not deleted")
	}
	attempts, err := w.Attempts()
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatal("history not deleted")
	}
}
