package orchestrator

import (
	"encoding/json"
	"slices"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
)

// Pin the shared action truth table independently of the implementation predicates.
func TestCardActionGuards(t *testing.T) {
	raw, err := json.Marshal(client.Outcome{Version: 1, State: runtime.RunFailed, TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote"})
	if err != nil {
		t.Fatal(err)
	}
	failed := Attempt{ID: "attempt", CardID: "card", Number: 2, TaskID: "task", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", State: "failed", RunState: "failed", OutcomeJSON: string(raw)}
	active := failed
	active.State, active.RunState = "running", "running"
	ready := failed
	ready.State, ready.RunState, ready.Ready, ready.PRURL = "ready", "completed", true, "https://github.com/owner/repo/pull/1"
	conflict := active
	conflict.Error = executionConflictNotice
	tests := []struct {
		name     string
		attempts []Attempt
		allowed  map[string][]string // action -> lanes
	}{
		{"legacy", nil, map[string][]string{"start": {"Building"}, "reset": {"Building", "Fix", "Done"}, "delete": {"Todo", "Planning", "Building", "Fix", "Done"}}},
		{"active", []Attempt{active}, map[string][]string{"cancel": {"Building", "Fix"}, "force-stop": {"Todo", "Building", "Fix", "Done"}}},
		{"failed", []Attempt{failed}, map[string][]string{"resume": {"Building", "Fix"}, "retry": {"Building", "Fix"}, "force-ready": {"Building", "Fix"}, "reset": {"Building", "Fix", "Done"}, "delete": {"Todo", "Planning", "Building", "Fix", "Done"}}},
		{"ready", []Attempt{ready}, map[string][]string{"fix": {"Done"}, "delete": {"Todo", "Planning", "Building", "Fix", "Done"}}},
		{"conflict", []Attempt{conflict}, map[string][]string{"delete": {"Todo", "Planning", "Building", "Fix", "Done"}, "cancel": {"Building", "Fix"}, "force-stop": {"Todo", "Building", "Fix", "Done"}}},
		{"older-active", []Attempt{ready, {ID: "older", CardID: "card", Number: 1, State: "queued"}}, map[string][]string{"force-stop": {"Todo", "Building", "Fix", "Done"}}},
		{"unrelated-active", []Attempt{ready, {ID: "other", CardID: "other", Number: 3, State: "queued"}}, map[string][]string{"fix": {"Done"}, "delete": {"Todo", "Planning", "Building", "Fix", "Done"}}},
	}
	for _, tt := range tests {
		for _, lane := range []string{"Todo", "Planning", "Building", "Fix", "Done"} {
			for _, action := range []string{"start", "resume", "retry", "fix", "reset", "delete", "cancel", "force-stop", "force-ready", "unknown"} {
				t.Run(tt.name+"/"+lane+"/"+action, func(t *testing.T) {
					want := slices.Contains(tt.allowed[action], lane)
					if got := canAction(Card{ID: "card", Status: lane}, tt.attempts, action); got != want {
						t.Fatalf("canAction = %v, want %v", got, want)
					}
				})
			}
		}
	}
}

func TestMergeEligibilityRequiresFixGuard(t *testing.T) {
	c := Card{ID: "card", Status: "Done"}
	a := Attempt{ID: "attempt", CardID: c.ID, State: "ready", Ready: true, PRURL: "https://github.com/owner/repo/pull/1"}
	check := PRCheck{AttemptID: a.ID, PRURL: a.PRURL, State: "clear", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	if !canAction(c, []Attempt{a}, "fix") || !mergeEligible(c, []Attempt{a}, check, false) {
		t.Fatal("expected eligible baseline")
	}
	for _, field := range []string{"ready", "state", "pr", "active"} {
		t.Run(field, func(t *testing.T) {
			changed := a
			switch field {
			case "ready":
				changed.Ready = false
			case "state":
				changed.State = "failed"
			case "pr":
				changed.PRURL = ""
			case "active":
				changed.RunState = "running"
			}
			if canFix(c, []Attempt{changed}, cardHasActiveAttempt(c.ID, []Attempt{changed})) {
				t.Fatal("fix guard must reject")
			}
			if mergeEligible(c, []Attempt{changed}, check, false) {
				t.Fatal("merge must reject when fix guard rejects")
			}
		})
	}
}
