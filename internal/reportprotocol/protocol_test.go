package reportprotocol

import (
	"strings"
	"testing"
)

func TestCanonicalStableIdentity(t *testing.T) {
	id := Identity{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn"}
	r, err := NewRequest(id, ProposeSpec, Payload{Spec: "  proposal  ", Questions: []string{" Question "}, RepoCommit: strings.Repeat("A", 40)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	id.AttemptID = "another-attempt"
	id.TurnID = "another-turn"
	retry, err := NewRequest(id, ProposeSpec, r.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if retry.ReportID != r.ReportID {
		t.Fatal("identity changed payload retry ID")
	}
	changed, _ := NewRequest(id, ProposeSpec, Payload{Spec: "different", Questions: r.Payload.Questions, RepoCommit: r.Payload.RepoCommit})
	if changed.ReportID == r.ReportID {
		t.Fatal("different payload shares hash")
	}
	r.Payload.Spec = "tampered"
	if r.Validate() == nil {
		t.Fatal("tampered report accepted")
	}
}
func TestRejectInvalidReports(t *testing.T) {
	id := Identity{"task", "agent", "run", "attempt", "turn"}
	for _, tc := range []struct {
		kind Kind
		p    Payload
	}{
		{CompleteTask, Payload{PRNumber: 0}},
		{CompleteTask, Payload{PRNumber: 1, Spec: "planning"}},
		{ProposeSpec, Payload{Spec: "spec", RepoCommit: "short"}},
		{ReportBlocker, Payload{Reason: " "}},
		{ReportBlocker, Payload{Reason: "blocked", PRNumber: 1}},
		{"unknown", Payload{}},
	} {
		if _, err := NewRequest(id, tc.kind, tc.p); err == nil {
			t.Fatalf("accepted invalid %+v", tc)
		}
	}
	for _, id := range []Identity{{TaskID: "task"}, {TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt"}} {
		if _, err := NewRequest(id, CompleteTask, Payload{PRNumber: 1}); err == nil {
			t.Fatal("accepted missing trusted identity")
		}
	}
}
