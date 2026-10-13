package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	channel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportbridge"
	"jawa/internal/testmodel"
)

// Real KIT + BONNIE SQLite journal + Bridge + JetStream + production DB ACKs.
// A build blocker parks the run; guidance resumes the SAME run after restart.
func TestBuildBlockerGuidanceResumesSameRun(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	model := testmodel.New(
		testmodel.Call("reportBlocker", `{"reason":"Need the staging API token"}`),
		testmodel.Call("completeTask", `{"pr_number":7}`),
	)
	dir := filepath.Join(t.TempDir(), "journal")
	var journal *runtime.SQLiteJournal
	var runner *runtime.Runner
	var ch bonnie.Channel
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
		runner = runtime.NewRunner(journal, factory)
		ch, err = b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "build-agent", CreateStream: true, TargetedTasks: true})
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
	defer func() { stop() }()
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	// The blocker is durable and the run parks with a trusted answer route.
	waitWorkflow(t, func() bool {
		var n int
		_ = s.db.QueryRow(`SELECT count(*) FROM workflow_interactions WHERE status='pending' AND waiting_json<>''`).Scan(&n)
		a, e := w.CardResult(c.ID)
		return n == 1 && e == nil && a.State == "blocked" && a.RunState == "waiting"
	})
	blocked, err := w.CardResult(c.ID)
	if err != nil || blocked.Error != "Need the staging API token" || terminalFailure(blocked) || blocked.OutcomeJSON != "" {
		t.Fatal(blocked, err)
	}
	guidance, err := s.blockerGuidance()
	if err != nil || guidance[c.ID].Reason != "Need the staging API token" || guidance[c.ID].Queued {
		t.Fatal(guidance, err)
	}
	if err = w.Retry(c.ID); err == nil {
		t.Fatal("fresh retry replaced a run awaiting guidance")
	}
	snap, err := runner.Snapshot(t.Context(), blocked.RunID)
	if err != nil || snap.State != runtime.RunWaiting || snap.Suspend == nil || snap.Suspend.Prompt != "Need the staging API token" {
		t.Fatal(snap, err)
	}

	// Both owners restart while parked; guidance still targets the same run.
	stop()
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if w, err = NewWorkflow(t.Context(), s, nc); err != nil {
		t.Fatal(err)
	}
	start()
	if snap, err = runner.Snapshot(t.Context(), blocked.RunID); err != nil || snap.State != runtime.RunWaiting {
		t.Fatal(snap, err)
	}
	if err = w.GuideBlocked(c.ID, "Use the token in vault path ci/staging", "stale"); err == nil {
		t.Fatal("stale guidance accepted")
	}
	if err = w.GuideBlocked(c.ID, "  ", guidance[c.ID].Token); err == nil {
		t.Fatal("empty guidance accepted")
	}
	results, err := nc.SubscribeSync("bonnie.results")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = results.Unsubscribe() }()
	if err = w.GuideBlocked(c.ID, "Use the token in vault path ci/staging", guidance[c.ID].Token); err != nil {
		t.Fatal(err)
	}
	if err = w.GuideBlocked(c.ID, "again", guidance[c.ID].Token); err == nil {
		t.Fatal("blocker answered twice")
	}
	waitWorkflow(t, func() bool { a, e := w.CardResult(c.ID); return e == nil && a.PRURL != "" })
	// Let the host publish the resumed run's final outcome before shutdown.
	for deadline := time.Now().Add(8 * time.Second); ; {
		msg, e := results.NextMsg(time.Until(deadline))
		if e != nil {
			t.Fatal("resumed run outcome not published", e)
		}
		var out client.Outcome
		if json.Unmarshal(msg.Data, &out) == nil && out.TaskID == blocked.TaskID && out.State == runtime.RunCompleted {
			break
		}
	}
	a, err := w.CardResult(c.ID)
	if err != nil || a.TaskID != blocked.TaskID || a.RunID != blocked.RunID || a.RemoteAttemptID != blocked.RemoteAttemptID || a.Error != "awaiting verification" || a.Ready {
		t.Fatal(a, err)
	}
	var status string
	if err = s.db.QueryRow(`SELECT status FROM workflow_interactions WHERE task_id=?`, a.TaskID).Scan(&status); err != nil || status != "answered" {
		t.Fatal(status, err)
	}
	var distinct, reports int
	_ = s.db.QueryRow(`SELECT count(DISTINCT run_id || ':' || attempt_id),count(*) FROM typed_reports WHERE task_id=?`, a.TaskID).Scan(&distinct, &reports)
	if distinct != 1 || reports != 2 {
		t.Fatal("guidance changed execution identity", distinct, reports)
	}
	// The resumed request carries the original task and the operator guidance.
	requests := model.Requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Text(), "Use the token in vault path ci/staging") || !strings.Contains(requests[1].Text(), "Implement this card") {
		t.Fatal("guidance did not continue the conversation", requests)
	}
	if guidance, err = s.blockerGuidance(); err != nil || len(guidance) != 0 {
		t.Fatal(guidance, err)
	}
}
