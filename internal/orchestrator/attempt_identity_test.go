package orchestrator

import (
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/orchestrator/db/sqlc"
)

func TestIdentityMatches(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, agent, run, remote string
		want                             bool
	}{
		{name: "unpinned", want: true},
		{name: "targeted same agent", target: "agent", want: true},
		{name: "pinned same", agent: "agent", run: "run", remote: "remote", want: true},
		{name: "pinned other agent", agent: "other", run: "run", remote: "remote"},
		{name: "pinned other run", agent: "agent", run: "other", remote: "remote"},
		{name: "pinned other attempt", agent: "agent", run: "run", remote: "other"},
		{name: "targeted other agent", target: "other"},
		{name: "target overrides matching pin", target: "other", agent: "agent", run: "run", remote: "remote"},
		{name: "partial pin", agent: "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Attempt{TargetAgentID: tc.target, AgentID: tc.agent, RunID: tc.run, RemoteAttemptID: tc.remote}
			turn := sqlc.PlanningTurn{TargetAgentID: tc.target, AgentID: tc.agent, RunID: tc.run, RemoteAttemptID: tc.remote}
			if got := identityMatches(tc.target, tc.agent, tc.run, tc.remote, "agent", "run", "remote"); got != tc.want {
				t.Fatalf("identityMatches = %v, want %v", got, tc.want)
			}
			if matches(a, "agent", "run", "remote") != tc.want || planningIdentityMatches(turn, "agent", "run", "remote") != tc.want {
				t.Fatal("build/planning wrappers disagree with identity predicate")
			}
		})
	}
}

func TestCurrentBuildAttempt(t *testing.T) {
	t.Parallel()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		lane   string
		number int
		want   bool
	}{
		{"Building", a.Number, true},
		{"Fix", a.Number, true},
		{"Building", a.Number - 1, false},
		{"Todo", a.Number, false},
		{"Done", a.Number, false},
	} {
		mustExec(t, w, `UPDATE cards SET status=? WHERE id=?`, tc.lane, c.ID)
		tx, err := w.s.writeTx()
		if err != nil {
			t.Fatal(err)
		}
		candidate := a
		candidate.Number = tc.number
		got, err := currentBuildAttempt(tx, candidate)
		_ = tx.Rollback()
		if err != nil || got != tc.want {
			t.Fatalf("lane=%s number=%d: current=%v err=%v, want %v", tc.lane, tc.number, got, err, tc.want)
		}
	}
}

func TestAttemptAdapters(t *testing.T) {
	t.Parallel()
	turn := sqlc.PlanningTurn{ID: "task", State: "failed", OutcomeJson: "outcome", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", Error: "failure", EventSeq: 7}
	a := planningAttempt(turn)
	want := Attempt{ID: "task", TaskID: "task", State: "failed", RunState: "failed", OutcomeJSON: "outcome", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", Error: "failure", EventSeq: 7}
	if a != want {
		t.Fatalf("planningAttempt = %+v, want %+v", a, want)
	}
	if got := a.target(); got != (client.Target{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote"}) {
		t.Fatalf("target = %+v", got)
	}
}

func TestCancellationUsesMigratedTable(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"cancel", "force stop"} {
		t.Run(action, func(t *testing.T) {
			// workflowFixture opens the database with OpenStore, running migrations.
			s, nc := workflowFixture(t)
			cl, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
			if err != nil {
				t.Fatal(err)
			}
			w := &Workflow{s: s, ctx: t.Context(), client: cl}
			c := workflowCard(t, s)
			if err := w.MoveCard(c.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, err := w.CardResult(c.ID)
			if err != nil {
				t.Fatal(err)
			}
			var tables int
			if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='workflow_cancel_requests'`).Scan(&tables); err != nil || tables != 1 {
				t.Fatalf("migration table count=%d err=%v", tables, err)
			}
			if action == "cancel" {
				mustExec(t, w, `UPDATE workflow_attempts SET agent_id='agent',run_id='run',remote_attempt_id='remote',state='running',run_state='running' WHERE id=?`, a.ID)
				target := client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}
				if err := replyJSON(nc, "bonnie.queries.agent", client.Status{Version: 1, Target: target, State: runtime.RunRunning, Active: true, TurnID: "turn"}); err != nil {
					t.Fatal(err)
				}
				if err := replyJSON(nc, "bonnie.commands.agent", client.Status{Version: 1, Target: target, State: runtime.RunRunning, TurnID: "turn", CancelStatus: "requested"}); err != nil {
					t.Fatal(err)
				}
				if err := nc.Flush(); err != nil {
					t.Fatal(err)
				}
				err = w.CancelCard(t.Context(), c.ID)
			} else {
				err = w.ForceStop(c.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var records int
			if err := s.db.QueryRow(`SELECT count(*) FROM workflow_cancel_requests WHERE task_id=?`, a.TaskID).Scan(&records); err != nil || records == 0 {
				t.Fatalf("cancellation audit count=%d err=%v", records, err)
			}
		})
	}
}
