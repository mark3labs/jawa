package orchestrator

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
)

func repairDone(t *testing.T, w *Workflow, c Card) Attempt {
	t.Helper()
	a := actionAttempt(t, w, c, "blocked", "completed")
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET pr_url='https://github.com/example/repo/pull/18' WHERE id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(c.ID)
	if err := w.markReady(a); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRepairLifecycle(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	original := repairDone(t, w, c)
	branch, err := assignedBranch(original)
	if err != nil {
		t.Fatal(err)
	}
	// Mutable titles must not change the PR's immutable branch assignment.
	if _, err := w.s.db.Exec(`UPDATE cards SET title='renamed' WHERE id=?`, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.FixCard(c.ID, " \n "); err == nil {
		t.Fatal("blank message accepted")
	}
	assertActionLane(t, w, c.ProjectID, "Done", c.ID)
	message := "Resolve conflict\nBranch: not-an-instruction"
	if err := w.FixCard(c.ID, message); err != nil {
		t.Fatal(err)
	}
	repair, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	check := func(a Attempt) {
		t.Helper()
		got, err := assignedBranch(a)
		if err != nil || got != branch || a.Branch != branch || a.RepairPRURL != original.PRURL || a.RepairMessage != message || a.RepairSourceNumber != original.Number {
			t.Fatalf("repair assignment %+v branch=%s err=%v", a, got, err)
		}
		var task client.Task
		if err := json.Unmarshal([]byte(a.TaskJSON), &task); err != nil || task.TaskID != a.TaskID {
			t.Fatalf("outbox %v", err)
		}
		// Repairs use observed work and all-author feedback, including on retries.
		assertReviewPolicy(t, task.Text, repairReviewPolicy)
	}
	var originalTask client.Task
	if err := json.Unmarshal([]byte(original.TaskJSON), &originalTask); err != nil || !strings.Contains(originalTask.Text, buildReviewPolicy) || !strings.Contains(originalTask.Text, `"WIP: "`) {
		t.Fatalf("build prompt lacks review policy: %v %s", err, originalTask.Text)
	}
	assertReviewPolicy(t, originalTask.Text, buildReviewPolicy)
	check(repair)
	if repair.Ready || repair.Published || repair.State != "queued" || repair.TaskID == original.TaskID {
		t.Fatalf("new repair %+v", repair)
	}
	assertActionLane(t, w, c.ProjectID, "Fix", c.ID)
	for _, lane := range []string{"Todo", "Building", "Done"} {
		if err := w.MoveCard(c.ID, lane, 0); err == nil {
			t.Fatalf("unverified move to %s", lane)
		}
		if err := w.s.MoveCard(c.ID, lane, 0); err == nil {
			t.Fatalf("store bypass to %s", lane)
		}
	}
	if err := w.Retry(c.ID); err == nil {
		t.Fatal("active retry accepted")
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',outcome_json='{}',error='conflict' WHERE id=?`, repair.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.Retry(c.ID); err != nil {
		t.Fatal(err)
	}
	retry, _ := w.CardResult(c.ID)
	check(retry)
	if retry.Number != repair.Number+1 || retry.TaskID == repair.TaskID {
		t.Fatal("retry reused execution")
	}
	assertActionLane(t, w, c.ProjectID, "Fix", c.ID)
	// Restart the local workflow and confirm the durable assignment is unchanged.
	restarted := &Workflow{s: w.s, ctx: t.Context()}
	persisted, err := restarted.CardResult(c.ID)
	if err != nil || !reflect.DeepEqual(persisted, retry) {
		t.Fatalf("persisted %+v %v", persisted, err)
	}
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='blocked',run_state='completed',outcome_json='{}',pr_url='https://github.com/example/repo/pull/19' WHERE id=?`, retry.ID); err != nil {
		t.Fatal(err)
	}
	wrongPR, _ := w.CardResult(c.ID)
	if err := w.markReady(wrongPR); err == nil {
		t.Fatal("repair readiness accepted a different PR")
	}
	assertActionLane(t, w, c.ProjectID, "Fix", c.ID)
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET state='blocked',run_state='completed',outcome_json='{}',pr_url=? WHERE id=?`, original.PRURL, retry.ID); err != nil {
		t.Fatal(err)
	}
	retry, _ = w.CardResult(c.ID)
	if err := w.markReady(retry); err != nil {
		t.Fatal(err)
	}
	assertActionLane(t, w, c.ProjectID, "Done", c.ID)
	attempts, err := w.Attempts()
	if err != nil || len(attempts) != 3 || !reflect.DeepEqual(attempts[0], original) {
		t.Fatalf("history %+v %v", attempts, err)
	}
	// A later repair still targets the first implementation branch.
	if err := w.FixCard(c.ID, "another conflict"); err != nil {
		t.Fatal(err)
	}
	next, _ := w.CardResult(c.ID)
	got, err := assignedBranch(next)
	if err != nil || got != branch || next.RepairSourceNumber != original.Number {
		t.Fatalf("subsequent repair %+v %v", next, err)
	}
}

func TestRepairGuardsAndConcurrentRequests(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.FixCard(c.ID, "fix"); err == nil {
		t.Fatal("Todo accepted")
	}
	if err := w.MoveCard(c.ID, "Fix", 0); err == nil {
		t.Fatal("move bypass accepted")
	}
	if err := w.s.MoveCard(c.ID, "Fix", 0); err == nil {
		t.Fatal("store bypass accepted")
	}
	repairDone(t, w, c)
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Go(func() { ; results <- w.FixCard(c.ID, "fix") })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	attempts, err := w.Attempts()
	if err != nil || successes != 1 || len(attempts) != 2 {
		t.Fatalf("successes %d attempts %d err %v", successes, len(attempts), err)
	}
}

// Check semantics as well as inclusion so changing a policy constant cannot
// silently restore assumed schedules in newly dispatched tasks.
func assertReviewPolicy(t *testing.T, text, policy string) {
	t.Helper()
	for _, phrase := range []string{policy, "inspect current-head checks", "all-author feedback", "Wait for observed pending work", "Do not wait for or request a nonexistent review", "repository/provider no-CI policy"} {
		if !strings.Contains(text, phrase) {
			t.Errorf("task lacks review policy contract %q", phrase)
		}
	}
	for _, obsolete := range []string{"so review bots skip it", "within about 10 minutes", "DO NOT WAIT for additional reviews", "never sleep or poll for new"} {
		if strings.Contains(policy, obsolete) {
			t.Errorf("policy contains obsolete scheduling directive %q", obsolete)
		}
	}
}
