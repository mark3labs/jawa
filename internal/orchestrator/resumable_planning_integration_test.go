package orchestrator

import (
	"context"
	"sync"

	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/bonnie"
	channel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportbridge"
	"jawa/internal/testmodel"
)

// Real KIT + BONNIE SQLite journal + Bridge + JetStream + production DB ACKs.
// Restart both owners while waiting, reject/revise, approve, then finish planning.
func TestResumablePlanningRealRuntimeRestartReviewAndBuild(t *testing.T) {
	skipFlaky(t)
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	model := testmodel.New(
		testmodel.Call("clarify", `{"questions":["Which API?"]}`),
		testmodel.Call("proposeSpec", `{"spec":"Use API v2","repo_commit":"`+strings.Repeat("a", 40)+`"}`),
		testmodel.Call("proposeSpec", `{"spec":"Use API v3","repo_commit":"`+strings.Repeat("a", 40)+`"}`),
		testmodel.Say("Planning approved and finished."),
	)
	buildModel := testmodel.New(testmodel.Call("completeTask", `{"pr_number":7}`))
	dir := filepath.Join(t.TempDir(), "journal")
	var journal *runtime.SQLiteJournal
	var runner *runtime.Runner
	var ch bonnie.Channel
	var planningRun string
	var factoryMu sync.Mutex
	start := func() {
		var err error
		journal, err = runtime.OpenSQLiteJournal(dir)
		if err != nil {
			t.Fatal(err)
		}
		b := &reportbridge.Bridge{}
		factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, session *runtime.Session) error {
			return b.Setup(ctx, k, bonnie.RunScope{RunID: session.RunID(), Session: session})
		}, model.Option(), func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
		})
		buildFactory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, session *runtime.Session) error {
			return b.Setup(ctx, k, bonnie.RunScope{RunID: session.RunID(), Session: session})
		}, buildModel.Option(), func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
		})
		selectFactory := func(ctx context.Context, session *runtime.Session) (runtime.Agent, error) {
			factoryMu.Lock()
			if planningRun == "" {
				planningRun = session.RunID()
			}
			planning := planningRun == session.RunID()
			factoryMu.Unlock()
			if !planning {
				return buildFactory(ctx, session)
			}
			return factory(ctx, session)
		}
		runner = runtime.NewRunner(journal, selectFactory)
		ch, err = b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "planning-agent", CreateStream: true, TargetedTasks: true})
		if err != nil {
			t.Fatal(err)
		}
		if err = ch.(interface{ Start(context.Context) error }).Start(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() {
		if err := ch.(interface{ Shutdown(context.Context) error }).Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	}
	start()
	defer stop()
	if err = w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		d, e := w.PlanningDetail(c.ID)
		return e == nil && d.Session.State == "questions" && d.InteractionToken != ""
	})
	d := planningDetails(t, w, c.ID)
	task := d.Turns[0].ID
	if len(d.Revisions) != 0 || d.Session.Spec != "" || d.CanApprove {
		t.Fatal("clarify fabricated proposal", d)
	}
	waitWorkflow(t, func() bool {
		var n int
		_ = s.db.QueryRow(`SELECT count(*) FROM planning_interactions WHERE task_id=? AND waiting_json<>''`, task).Scan(&n)
		return n == 1
	})
	first := storedPlanningWaiting(t, w, task)
	stop()
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	start()
	snap, err := runner.Snapshot(t.Context(), first.RunID)
	if err != nil || snap.State != runtime.RunWaiting || snap.Suspend.ToolCallID != first.Suspend.ToolCallID {
		t.Fatal(snap, err)
	}
	if err = w.ReplyPlanning(c.ID, "API v2", "stale"); err == nil {
		t.Fatal("stale reply accepted")
	}
	if err = w.ReplyPlanning(c.ID, "API v2", d.InteractionToken); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		d, e := w.PlanningDetail(c.ID)
		return e == nil && d.CanApprove && d.Session.Revision == 1
	})
	d = planningDetails(t, w, c.ID)
	if len(d.Turns) != 1 || d.Turns[0].ID != task {
		t.Fatal("reply created task", d)
	}
	if err = w.ReplyPlanning(c.ID, "Use API v3 instead", d.InteractionToken); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		d, e := w.PlanningDetail(c.ID)
		return e == nil && d.CanApprove && d.Session.Revision == 2
	})
	if err = w.ApprovePlanning(c.ID, 1); err == nil {
		t.Fatal("stale approval accepted")
	}
	if err = w.ApprovePlanning(c.ID, 2); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		var state string
		_ = s.db.QueryRow(`SELECT state FROM planning_turns WHERE id=?`, task).Scan(&state)
		return state == "completed"
	})
	waitWorkflow(t, func() bool { a, e := w.CardResult(c.ID); return e == nil && a.PRURL != "" })
	d = planningDetails(t, w, c.ID)
	a, err := w.CardResult(c.ID)
	if err != nil || a.TaskID == task || d.Session.ApprovedSpec != "Use API v3" || d.Session.ApprovedRevision != 2 || !strings.Contains(a.TaskJSON, "Use API v3") {
		t.Fatal(d, a, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM typed_reports WHERE task_id=?`, task).Scan(&count); err != nil || count != 3 {
		t.Fatal("interaction ledger", count, err)
	}
	// Check authoritative stored identities, not model-generated natural text.
	var distinct int
	_ = s.db.QueryRow(`SELECT count(DISTINCT run_id || ':' || attempt_id) FROM typed_reports WHERE task_id=?`, task).Scan(&distinct)
	if distinct != 1 {
		t.Fatal("reply changed execution identity", distinct)
	}
	requests := model.Requests()
	for i, text := range []string{"API v2", "Use API v3 instead", "Approved."} {
		if len(requests) < i+2 || !strings.Contains(requests[i+1].Text(), text) || !strings.Contains(requests[i+1].Text(), "READ-ONLY") {
			t.Fatal("lost conversation", i, requests)
		}
	}
	// Approval completion is not a build report and cannot bypass typed verification.
	if a.Ready || a.Error != "awaiting verification" {
		t.Fatal(a)
	}
}
