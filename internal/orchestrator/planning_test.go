package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
	"time"
)

func planningLocal(t *testing.T) (*Store, *Workflow, Card) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w := &Workflow{s: s, ctx: context.Background()}
	return s, w, workflowCard(t, s)
}
func planningDetails(t *testing.T, w *Workflow, id string) PlanningDetail {
	t.Helper()
	d, e := w.PlanningDetail(id)
	if e != nil {
		t.Fatal(e)
	}
	return d
}
func planningOutcome(t *testing.T, w *Workflow, id, spec string, questions ...string) client.Outcome {
	t.Helper()
	d := planningDetails(t, w, id)
	if questions == nil {
		questions = []string{}
	}
	body, e := json.Marshal(reportprotocol.Payload{Spec: spec, Questions: questions, RepoCommit: strings.Repeat("a", 40)})
	if e != nil {
		t.Fatal(e)
	}
	return client.Outcome{Version: 1, TaskID: d.Turns[len(d.Turns)-1].ID, AgentID: "planner", RunID: "run", AttemptID: "attempt", State: runtime.RunCompleted, Response: string(body)}
}
func TestPlanningBackendConversationApprovalAndRetrySnapshot(t *testing.T) {
	s, w, c := planningLocal(t)
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e := w.ApprovePlanning(c.ID, 0); e == nil {
		t.Fatal("approved queued turn")
	}
	if e := w.MoveCard(c.ID, "Building", 0); e == nil {
		t.Fatal("bypassed approval")
	}
	if e := w.ReplyPlanning(c.ID, "premature", planningDetails(t, w, c.ID).InteractionToken); e == nil {
		t.Fatal("reply while active")
	}
	first := planningOutcome(t, w, c.ID, "Draft spec", "Which API?")
	if e := planningDelivery(t, w, first); e != nil {
		t.Fatal(e)
	}
	if e := planningDelivery(t, w, first); e != nil {
		t.Fatal(e)
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.Revision != 0 || len(d.Messages) != 2 || len(d.Questions) != 1 || d.CanApprove {
		t.Fatalf("questions snapshot: %#v", d)
	}
	if e := w.ApprovePlanning(c.ID, 1); e == nil {
		t.Fatal("approved questions")
	}
	if e := w.ReplyPlanning(c.ID, "Use API v2", planningDetails(t, w, c.ID).InteractionToken); e != nil {
		t.Fatal(e)
	}
	second := planningOutcome(t, w, c.ID, "Approved spec v2")
	if e := planningDelivery(t, w, second); e != nil {
		t.Fatal(e)
	}
	if e := w.ApprovePlanning(c.ID, 0); e == nil {
		t.Fatal("approved stale revision")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { ; errs <- w.ApprovePlanning(c.ID, 1) })
	}
	wg.Wait()
	close(errs)
	success := 0
	for e := range errs {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("approval successes %d", success)
	}
	d = planningDetails(t, w, c.ID)
	if d.Session.ApprovedSpec != "Approved spec v2" || d.Session.ApprovedRevision != 1 || d.Session.State != "approved" {
		t.Fatal(d.Session)
	}
	cards, e := s.Cards(c.ProjectID)
	if e != nil || cards[0].Status != "Building" {
		t.Fatal(cards, e)
	}
	a, e := w.CardResult(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(a.TaskJSON, "Approved spec v2") {
		t.Fatal("approved spec not in build task")
	}
	if e = planningDelivery(t, w, client.Outcome{TaskID: a.TaskID, AgentID: "builder", RunID: "b", AttemptID: "b", State: runtime.RunFailed, Error: "test failed"}); e != nil {
		t.Fatal(e)
	}
	if e = w.Retry(c.ID); e != nil {
		t.Fatal(e)
	}
	a, e = w.CardResult(c.ID)
	if e != nil || !strings.Contains(a.TaskJSON, "Approved spec v2") {
		t.Fatal("retry lost approved spec", e)
	}
	if e = planningDelivery(t, w, first); e != nil {
		t.Fatal(e)
	}
	if planningDetails(t, w, c.ID).Session.Revision != 1 {
		t.Fatal("late planning result changed approved revision")
	}
}

func TestPlanningBackendCancellationNewSessionAndLateIdentity(t *testing.T) {
	_, w, c := planningLocal(t)
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	old := planningOutcome(t, w, c.ID, "old spec")
	before := planningDetails(t, w, c.ID)
	if e := w.CancelPlanning(c.ID); e != nil {
		t.Fatal(e)
	}
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e := planningDelivery(t, w, old); e == nil {
		t.Fatal("late typed report accepted")
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.ID == before.Session.ID || d.Session.Revision != 0 {
		t.Fatal("late result revived prior session")
	}
	out := planningOutcome(t, w, c.ID, "real spec")
	ev := client.StatusEvent{TaskID: out.TaskID, AgentID: "planner", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning, Seq: 4}
	if e := w.event(t.Context(), ev); e != nil {
		t.Fatal(e)
	}
	bad := out
	bad.AgentID = "other"
	if e := planningDelivery(t, w, bad); e == nil {
		t.Fatal("wrong owner accepted")
	}
	if planningDetails(t, w, c.ID).Session.Revision != 0 {
		t.Fatal("mismatched identity accepted")
	}
	if e := planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	ev.State = runtime.RunFailed
	ev.Seq = 5
	if e := w.event(t.Context(), ev); e != nil {
		t.Fatal(e)
	}
	if !planningDetails(t, w, c.ID).CanApprove {
		t.Fatal("late event undid terminal outcome")
	}
}

func TestPlanningBackendInvalidOutputAndPinnedCommit(t *testing.T) {
	_, w, c := planningLocal(t)
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	out := planningOutcome(t, w, c.ID, "spec")
	out.Response = `{"pr_number":123}`
	if e := planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.State != "failed" || d.Session.Revision != 0 {
		t.Fatal(d.Session)
	}
	if e := w.RetryPlanning(c.ID); e != nil {
		t.Fatal(e)
	}
	out = planningOutcome(t, w, c.ID, "spec")
	if e := planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	if e := w.ReplyPlanning(c.ID, "Please refine", planningDetails(t, w, c.ID).InteractionToken); e != nil {
		t.Fatal(e)
	}
	out = planningOutcome(t, w, c.ID, "different")
	out.Response = strings.ReplaceAll(out.Response, strings.Repeat("a", 40), strings.Repeat("b", 40))
	if e := planningDelivery(t, w, out); e == nil {
		t.Fatal("different pinned commit accepted")
	}
	d = planningDetails(t, w, c.ID)
	if d.Session.State != "running" || d.Session.Revision != 1 || d.Session.Spec != "spec" {
		t.Fatal(d.Session)
	}
}

func TestPlanningBackendQueuedRestartBONNIEIntegration(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	local := &Workflow{s: s, ctx: context.Background()}
	if e := local.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	d := planningDetails(t, local, c.ID)
	taskID := d.Turns[0].ID
	w, e := NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	waitWorkflow(t, func() bool { d, e := w.PlanningDetail(c.ID); return e == nil && d.Turns[0].Published })
	js, e := nc.JetStream()
	if e != nil {
		t.Fatal(e)
	}
	sub, e := js.PullSubscribe("bonnie.tasks", "planning-smoke-agent")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = sub.Unsubscribe() }()
	msgs, e := sub.Fetch(1, nats.MaxWait(5*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	var received client.Task
	if e = json.Unmarshal(msgs[0].Data, &received); e != nil {
		t.Fatal(e)
	}
	if received.TaskID != taskID || received.Version != 1 {
		t.Fatal("wrong wire task", received)
	}
	if e = msgs[0].AckSync(); e != nil {
		t.Fatal(e)
	}
	turn, e := sqlc.New(s.db).GetPlanningTurn(t.Context(), taskID)
	if e != nil {
		t.Fatal(e)
	}
	var task client.Task
	if e = json.Unmarshal([]byte(turn.TaskJson), &task); e != nil {
		t.Fatal(e)
	}
	if received.Text != task.Text {
		t.Fatal("wire task differs from durable outbox")
	}
	for _, text := range []string{"READ-ONLY", "NOT implementing", "repo_commit", "Conversation JSON"} {
		if !strings.Contains(task.Text, text) {
			t.Fatalf("missing planner contract %q", text)
		}
	}
	out := planningOutcome(t, w, c.ID, "Persisted spec", "Question?")
	if e = planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	publishOutcome(t, nc, storedPlanningWaiting(t, w, out.TaskID))
	waitWorkflow(t, func() bool { d, e := w.PlanningDetail(c.ID); return e == nil && d.Session.State == "questions" })
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	local = &Workflow{s: s, ctx: context.Background()}
	if e = local.ReplyPlanning(c.ID, "answer", planningDetails(t, local, c.ID).InteractionToken); e != nil {
		t.Fatal(e)
	}
	w, e = NewWorkflow(t.Context(), s, nc)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = w.Close() }()
	waitWorkflow(t, func() bool {
		d, e := w.PlanningDetail(c.ID)
		return e == nil && len(d.Turns) == 1 && d.Turns[0].Published
	})
	out = planningOutcome(t, w, c.ID, "Final spec")
	if e = planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	publishOutcome(t, nc, storedPlanningWaiting(t, w, out.TaskID))
	waitWorkflow(t, func() bool {
		d, e := w.PlanningDetail(c.ID)
		return e == nil && d.CanApprove && d.Session.Revision == 1
	})
	if e = w.ApprovePlanning(c.ID, 1); e != nil {
		t.Fatal(e)
	}
}

func TestPlanningBackendApprovalTransactionRollsBack(t *testing.T) {
	s, w, c := planningLocal(t)
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e := planningDelivery(t, w, planningOutcome(t, w, c.ID, "spec")); e != nil {
		t.Fatal(e)
	}
	if _, e := s.db.Exec(`CREATE TRIGGER deny_build BEFORE UPDATE OF status ON cards WHEN NEW.status='Building' BEGIN SELECT RAISE(ABORT,'no'); END`); e != nil {
		t.Fatal(e)
	}
	if e := w.ApprovePlanning(c.ID, 1); e == nil {
		t.Fatal("expected trigger rejection")
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.State != "ready" || d.Session.ApprovedSpec != "" {
		t.Fatal("approval partially committed")
	}
	as, e := w.Attempts()
	if e != nil || len(as) != 0 {
		t.Fatal("outbox partially committed", as, e)
	}
}

func TestPlanningBackendRevisionTokenAcrossSessions(t *testing.T) {
	_, w, c := planningLocal(t)
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e := planningDelivery(t, w, planningOutcome(t, w, c.ID, "old approved-looking spec")); e != nil {
		t.Fatal(e)
	}
	if e := w.CancelPlanning(c.ID); e != nil {
		t.Fatal(e)
	}
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e := planningDelivery(t, w, planningOutcome(t, w, c.ID, "new spec")); e != nil {
		t.Fatal(e)
	}
	if e := w.ApprovePlanning(c.ID, 1); e == nil {
		t.Fatal("approval from cancelled session reused revision token")
	}
	if planningDetails(t, w, c.ID).Session.Revision != 2 {
		t.Fatal("revision token must be card-global")
	}
	if e := w.ApprovePlanning(c.ID, 2); e != nil {
		t.Fatal(e)
	}
	a, e := w.CardResult(c.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = assignedBranch(a); e != nil {
		t.Fatal("approved build task incompatible with verifier", e)
	}
	var task client.Task
	if e = json.Unmarshal([]byte(a.TaskJSON), &task); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(task.Text, "\nRepository: https://github.com/example/repo\nBase branch: main\nProvider: github\nIssue: ") {
		t.Fatal("approved task lacks provider verifier configuration tail")
	}
}

func TestPlanningBackendActualStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "planning.db")
	s, e := OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	w := &Workflow{s: s, ctx: context.Background()}
	c := workflowCard(t, s)
	if e = w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e = planningDelivery(t, w, planningOutcome(t, w, c.ID, "durable spec", "Need answer")); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = OpenStore(path)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = s.Close() }()
	w = &Workflow{s: s, ctx: context.Background()}
	d := planningDetails(t, w, c.ID)
	if d.Session.Spec != "" || len(d.Messages) != 2 || len(d.Questions) != 1 {
		t.Fatal("lost persisted conversation", d)
	}
	if e = w.ReplyPlanning(c.ID, "answer after reopen", planningDetails(t, w, c.ID).InteractionToken); e != nil {
		t.Fatal(e)
	}
	d = planningDetails(t, w, c.ID)
	if d.Session.State != "running" || len(d.Turns) != 1 || len(d.Messages) != 3 {
		t.Fatal(d)
	}
}

func TestPlanningBackendRetainedOutcomeRecovery(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w := &Workflow{s: s, ctx: context.Background()}
	if e := w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	// Provision actual BONNIE streams but do not start consumers: the retained
	// terminal report must be recovered through replay, not consumer delivery.
	cli, e := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true})
	if e != nil {
		t.Fatal(e)
	}
	w.client = cli
	w.dispatchPlanning()
	out := planningOutcome(t, w, c.ID, "Recovered spec")
	if e = planningDelivery(t, w, out); e != nil {
		t.Fatal(e)
	}
	publishOutcome(t, nc, storedPlanningWaiting(t, w, out.TaskID))
	if e = w.recoverPlanningResults(nc); e != nil {
		t.Fatal(e)
	}
	if e = w.recoverPlanningResults(nc); e != nil {
		t.Fatal(e)
	}
	d := planningDetails(t, w, c.ID)
	if !d.CanApprove || d.Session.Revision != 1 || len(d.Revisions) != 1 || len(d.Messages) != 2 {
		t.Fatal("recovery duplicated or lost outcome", d)
	}
}

func TestPlanningBackendCancellationQueueUsesObservedTurn(t *testing.T) {
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	cli, e := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true})
	if e != nil {
		t.Fatal(e)
	}
	w := &Workflow{s: s, ctx: context.Background(), client: cli}
	if e = w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	w.dispatchPlanning()
	out := planningOutcome(t, w, c.ID, "Ignored spec")
	if e = w.CancelPlanning(c.ID); e != nil {
		t.Fatal(e)
	}
	// Agent admission arrives only after cancellation. Keep its identity without
	// allowing its run event to revive the locally revoked session.
	ev := client.StatusEvent{TaskID: out.TaskID, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID, State: runtime.RunRunning, Seq: 1}
	if e = w.event(t.Context(), ev); e != nil {
		t.Fatal(e)
	}
	target := client.Target{TaskID: out.TaskID, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID}
	statusData, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunRunning, Active: true, TurnID: "observed-turn", Seq: 1})
	query, e := nc.Subscribe("bonnie.queries.planner", func(m *nats.Msg) { _ = m.Respond(statusData) })
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = query.Unsubscribe() }()
	commands := make(chan []byte, 2)
	replyData, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunRunning, Active: true, TurnID: "observed-turn", CancelRequested: true})
	command, e := nc.Subscribe("bonnie.commands.planner", func(m *nats.Msg) { commands <- m.Data; _ = m.Respond(replyData) })
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = command.Unsubscribe() }()
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	w.dispatchPlanningCancellations()
	select {
	case data := <-commands:
		var request struct {
			TurnID string `json:"turn_id"`
			client.Target
		}
		if e = json.Unmarshal(data, &request); e != nil {
			t.Fatal(e)
		}
		if request.TurnID != "observed-turn" || request.Target != target {
			t.Fatal("cancellation not scoped to exact observed turn", request)
		}
	case <-time.After(time.Second):
		t.Fatal("no cancellation command")
	}
	turn, e := sqlc.New(s.db).GetPlanningTurn(t.Context(), out.TaskID)
	if e != nil || turn.CancelSent != 1 || turn.State != "cancelled" {
		t.Fatal(turn, e)
	}
	if e = planningDelivery(t, w, out); e == nil {
		t.Fatal("cancelled report accepted")
	}
	d := planningDetails(t, w, c.ID)
	if d.Session.State != "cancelled" || d.Session.Revision != 0 {
		t.Fatal("cancelled planner revived", d)
	}
}

func TestPlanningBackendHistoryCascadesAndLaneCompacts(t *testing.T) {
	s, w, c := planningLocal(t)
	other, e := s.CreateCard(c.ProjectID, "other", "desc")
	if e != nil {
		t.Fatal(e)
	}
	if e = w.MoveCard(c.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e = w.MoveCard(other.ID, "Planning", 0); e != nil {
		t.Fatal(e)
	}
	if e = planningDelivery(t, w, planningOutcome(t, w, c.ID, "spec")); e != nil {
		t.Fatal(e)
	}
	if e = w.DeleteCard(c.ID); e == nil {
		t.Fatal("deleted active planning without cancellation")
	}
	if e = w.CancelPlanning(c.ID); e != nil {
		t.Fatal(e)
	}
	if e = w.DeleteCard(c.ID); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"planning_sessions", "planning_messages", "planning_turns", "planning_revisions", "planning_deliveries"} {
		var n int
		// Other card remains, but deleted card's completed revision/deliveries do not.
		query := "SELECT count(*) FROM " + table
		switch table {
		case "planning_sessions":
			query += " WHERE card_id='" + c.ID + "'"
		case "planning_revisions", "planning_deliveries":
		default:
			continue
		}
		if e = s.db.QueryRow(query).Scan(&n); e != nil || n != 0 {
			t.Fatalf("history did not cascade: %s count=%d %v", table, n, e)
		}
	}
	cards, e := s.Cards(c.ProjectID)
	if e != nil || len(cards) != 1 || cards[0].ID != other.ID || cards[0].Position != 0 || cards[0].Status != "Planning" {
		t.Fatal("lane did not compact", cards, e)
	}
}
