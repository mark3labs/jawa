package orchestrator

import "testing"

func TestActiveAttemptSQLAgreement(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := actionAttempt(t, w, c, "failed", "failed")
	// Include every durable/remote state and unknown values. Interrupted in either
	// field is deliberately active, even though current writers use blocked.
	states := []string{"", "queued", "submitted", "running", "waiting", "interrupted", "blocked", "failed", "ready", "completed", "cancelled", "abandoned", "pending", "unknown"}
	for _, state := range states {
		for _, run := range states {
			t.Run(state+"/"+run, func(t *testing.T) {
				tx, err := w.s.writeTx()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				if _, err := tx.Exec(`UPDATE workflow_attempts SET state=?,run_state=? WHERE id=?`, state, run, a.ID); err != nil {
					t.Fatal(err)
				}
				a.State, a.RunState = state, run
				err = noActiveAttempts(tx, c.ID)
				if got := attemptActive(a); got != (err != nil) {
					t.Fatalf("Go active=%v, SQL guard=%v", got, err)
				}
				if err != nil && err.Error() != "card has an active attempt; cancel it and wait for confirmed stop before resetting or deleting" {
					t.Fatal(err)
				}
				if cardHasActiveAttempt("other", []Attempt{a}) {
					t.Fatal("other card counted as active")
				}
			})
		}
	}
}

func TestResetEligibilityShared(t *testing.T) {
	for _, tc := range []struct {
		name, lane, state, run, notice  string
		history, activeHistory, allowed bool
	}{
		{name: "legacy", lane: "Building", allowed: true},
		{name: "todo", lane: "Todo"},
		{name: "planning", lane: "Planning"},
		{name: "failed", lane: "Building", state: "failed", run: "failed", history: true, allowed: true},
		{name: "fix", lane: "Fix", state: "blocked", run: "completed", history: true, allowed: true},
		{name: "ready", lane: "Done", state: "ready", run: "completed", history: true},
		{name: "interrupted", lane: "Building", state: "interrupted", history: true},
		{name: "waiting", lane: "Building", state: "blocked", run: "waiting", history: true},
		{name: "pending snapshot", lane: "Building", state: "failed", run: "failed", notice: resumePendingNotice, history: true},
		{name: "active history", lane: "Building", state: "failed", run: "failed", history: true, activeHistory: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := actionWorkflow(t)
			c := workflowCard(t, w.s)
			if tc.history {
				first := actionAttempt(t, w, c, tc.state, tc.run)
				if tc.activeHistory {
					if err := w.Retry(c.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',outcome_json='{}' WHERE card_id=?`, c.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='running' WHERE id=?`, first.ID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET error=? WHERE card_id=?`, tc.notice, c.ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := w.s.db.Exec(`UPDATE cards SET status=? WHERE id=?`, tc.lane, c.ID); err != nil {
				t.Fatal(err)
			}
			c.Status = tc.lane
			attempts, err := w.Attempts()
			if err != nil {
				t.Fatal(err)
			}
			reason := resetReason(c, attempts, false)
			if (reason == "") != tc.allowed || canAction(c, attempts, "reset") != tc.allowed || actionReason(c, attempts, "reset") != reason {
				t.Fatalf("allowed=%v reason=%q", tc.allowed, reason)
			}
			err = w.ResetCard(c.ID)
			if (err == nil) != tc.allowed {
				t.Fatalf("workflow allowed=%v: %v", tc.allowed, err)
			}
			if err != nil && err.Error() != reason {
				t.Fatalf("workflow=%v UI=%q", err, reason)
			}
		})
	}
	if reason := resetReason(Card{ID: "c", Status: "Building"}, nil, true); reason == "" {
		t.Fatal("pending reservation allowed")
	}
}

func TestResetPendingReservation(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := actionAttempt(t, w, c, "failed", "failed")
	// A pending command must block Reset even if its presentation marker is
	// absent from a stale attempt row. The workflow reads both in one transaction.
	if _, err := w.s.db.Exec(`INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('resume',?,?,0,'{}','{}',0)`, a.TaskID, c.ID); err != nil {
		t.Fatal(err)
	}
	c.Status = "Building"
	reason := resetReason(c, []Attempt{a}, true)
	if err := w.ResetCard(c.ID); err == nil || err.Error() != reason {
		t.Fatalf("pending reset: %v, want %q", err, reason)
	}
	assertActionLane(t, w, c.ProjectID, "Building", c.ID)
}
