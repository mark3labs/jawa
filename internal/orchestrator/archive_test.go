package orchestrator

import (
	"testing"
)

func TestSetCardArchivedPreservesWorkflowState(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	c := testCard(t, s, p, "archive me")
	if err := s.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCardArchived(c.ID, true); err != nil {
		t.Fatal(err)
	}
	cards, err := s.Cards(p.ID)
	if err != nil || len(cards) != 1 || !cards[0].Archived || cards[0].Status != "Building" || cards[0].Position != 0 {
		t.Fatalf("archived card %+v, err %v", cards, err)
	}
	if err := s.SetCardArchived(c.ID, false); err != nil {
		t.Fatal(err)
	}
	cards, err = s.Cards(p.ID)
	if err != nil || cards[0].Archived || cards[0].Status != "Building" {
		t.Fatalf("restored card %+v, err %v", cards, err)
	}
	if err := s.SetCardArchived("missing", true); err == nil {
		t.Fatal("expected missing card error")
	}
}
