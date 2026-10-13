package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	channel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportprotocol"
)

func TestFailedResumeHostFailClosed(t *testing.T) {
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	r := runtime.NewRunner(j, nil)
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	b := &Bridge{runner: r, cfg: cfg}
	target := client.Target{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt"}
	admission, _ := json.Marshal(target)
	_, _ = j.Append(ctx, runtime.Record{RunID: admissionKey(cfg), Kind: runtime.RecordExtensionData, ExtType: "nats_task_admitted", Payload: admission})
	_, _ = j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordTurn, Text: "turn"})
	seq, _ := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordState, State: runtime.RunFailed})
	valid := reportprotocol.FailedResume{Version: 1, ID: "resume", Target: target, TurnID: "turn", Seq: seq}
	for name, edit := range map[string]func(*reportprotocol.FailedResume){
		"stale cursor":  func(q *reportprotocol.FailedResume) { q.Seq-- },
		"stale turn":    func(q *reportprotocol.FailedResume) { q.TurnID = "old" },
		"unknown task":  func(q *reportprotocol.FailedResume) { q.Target.TaskID = "other" },
		"wrong agent":   func(q *reportprotocol.FailedResume) { q.Target.AgentID = "other" },
		"wrong attempt": func(q *reportprotocol.FailedResume) { q.Target.AttemptID = "other" },
		"version":       func(q *reportprotocol.FailedResume) { q.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			q := valid
			edit(&q)
			reply := b.admitResume(ctx, q)
			if reply.Status != "rejected" || reply.Error == "" {
				t.Fatal(reply)
			}
		})
	}
	// Crash after intent but before StartRetry is deliberately ambiguous. Neither
	// replay nor a new command identity may start the saved failed run blindly.
	raw, _ := json.Marshal(valid)
	_, _ = j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: raw})
	reply := b.admitResume(ctx, valid)
	if reply.Status != "ambiguous" {
		t.Fatal(reply)
	}
	changed := valid
	changed.ID = "new"
	changed.Seq++
	reply = b.admitResume(ctx, changed)
	if reply.Status != "rejected" {
		t.Fatal("ambiguous prior intent was bypassed", reply)
	}
	reply = b.admitResume(ctx, valid)
	if reply.Status != "ambiguous" || r.IsActive("run") {
		t.Fatal("replay executed ambiguous command", reply)
	}
}

func resumeFixture(t *testing.T, j runtime.Journal) (*Bridge, reportprotocol.FailedResume) {
	t.Helper()
	ctx := context.Background()
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	b := &Bridge{runner: runtime.NewRunner(j, nil), cfg: cfg}
	target := client.Target{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt"}
	admission, _ := json.Marshal(target)
	for _, rec := range []runtime.Record{
		{RunID: admissionKey(cfg), Kind: runtime.RecordExtensionData, ExtType: "nats_task_admitted", Payload: admission},
		{RunID: "run", Kind: runtime.RecordTurn, Text: "turn"},
		{RunID: "run", Kind: runtime.RecordState, State: runtime.RunFailed},
	} {
		if _, err := j.Append(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := j.Replay(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	return b, reportprotocol.FailedResume{Version: 1, ID: "resume", Target: target, TurnID: "turn", Seq: recs[len(recs)-1].Seq}
}

func appendResumeIntent(t *testing.T, j runtime.Journal, req reportprotocol.FailedResume) {
	t.Helper()
	raw, _ := json.Marshal(req)
	if _, err := j.Append(context.Background(), runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedResumeLocalInFlightProtection(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	appendResumeIntent(t, j, req)
	// Covers both the gap before StartRetry acquires the runner and the gap
	// after it releases the runner but before the receipt is saved.
	b.resumeInFlight = map[reportprotocol.FailedResume]bool{req: true}
	if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
		t.Fatal(reply)
	}
	other := req
	other.ID = "other"
	if reply := b.admitResume(context.Background(), other); reply.Error != "earlier resume remains unconfirmed" {
		t.Fatal(reply)
	}
	delete(b.resumeInFlight, req)
	if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" {
		t.Fatal(reply)
	}
}

func TestFailedResumeActiveRunnerProtection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	appendResumeIntent(t, j, req)
	entered, release := make(chan struct{}), make(chan struct{})
	b.runner = runtime.NewRunner(j, func(ctx context.Context, s *runtime.Session) (runtime.Agent, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil, errors.New("test execution failure")
	})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = b.runner.Start(ctx, "run", runtime.Input{Text: "test"})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if reply := b.admitResume(ctx, req); reply.Status != "pending" {
		t.Error(reply)
	}
	reconciliation := req
	reconciliation.Reconcile = true
	if reply := b.admitResume(ctx, reconciliation); reply.Status != "pending" {
		t.Error("active existing intent reconciled", reply)
	}
	reconciliation.ID = "unseen"
	if reply := b.admitResume(ctx, reconciliation); reply.Status != "pending" {
		t.Error("active unseen command reconciled", reply)
	}
	other := req
	other.ID = "other"
	if reply := b.admitResume(ctx, other); reply.Error != "earlier resume remains unconfirmed" {
		t.Error(reply)
	}
	appendLegacyResumeReceipt(t, j, req, func(v *reportprotocol.FailedResumeReply) { v.Seq = 0 })
	reconciliation = req
	reconciliation.Reconcile = true
	if reply := b.admitResume(ctx, reconciliation); reply.Status != "pending" {
		t.Error("active legacy completed receipt upgraded", reply)
	}
	close(release)
	<-finished
	if reply := b.admitResume(ctx, reconciliation); reply.Status != "ambiguous" {
		t.Fatal(reply)
	}
}

func TestFailedResumeNewRequestRecoversOrphanBeforeCAS(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	appendResumeIntent(t, j, req)
	other := req
	other.ID = "new"
	other.Seq++ // Current cursor includes the orphan intent, not its receipt.
	if reply := b.admitResume(context.Background(), other); reply.Status != "rejected" || reply.Error != "run changed or is not an idle execution failure" {
		t.Fatal(reply)
	}
	recs, _ := j.Replay(context.Background(), "run")
	if recs[len(recs)-1].ExtType != resumeReceiptType {
		t.Fatal("orphan receipt not persisted")
	}
	if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" {
		t.Fatal(reply)
	}
	other.Seq = recs[len(recs)-1].Seq
	if reply := b.admitResume(context.Background(), other); reply.Status != "pending" {
		t.Fatal("recovered orphan still blocks new request", reply)
	}
	b.resumeWG.Wait()
}

// Inject faults only into receipt persistence, leaving admission replay intact.
type resumeFaultJournal struct {
	snapshotFailure bool
	runReplays      int
	runtime.Journal
	mu                             sync.Mutex
	appendFailures, replayFailures int
	appendCalls, replayCalls       int
	replayFaults                   bool
	invalidCursor                  bool
}

func (j *resumeFaultJournal) Append(ctx context.Context, rec runtime.Record) (int, error) {
	if rec.ExtType == resumeReceiptType {
		j.mu.Lock()
		j.appendCalls++
		fail := j.appendFailures > 0
		if fail {
			j.appendFailures--
		}
		j.mu.Unlock()
		if fail {
			return 0, errors.New("receipt append failed")
		}
		if _, ok := ctx.Deadline(); !ok {
			return 0, errors.New("unbounded append")
		}
	}
	return j.Journal.Append(ctx, rec)
}

func (j *resumeFaultJournal) Replay(ctx context.Context, id string) ([]runtime.Record, error) {
	if j.snapshotFailure && id == "run" {
		j.runReplays++
		if j.runReplays >= 2 {
			return nil, errors.New("snapshot unavailable")
		}
	}
	if j.replayFaults {
		j.mu.Lock()
		j.replayCalls++
		fail := j.replayFailures > 0
		if fail {
			j.replayFailures--
		}
		j.mu.Unlock()
		if _, ok := ctx.Deadline(); !ok {
			return nil, errors.New("unbounded replay")
		}
		if fail {
			return nil, errors.New("outcome replay failed")
		}
		if j.invalidCursor {
			return []runtime.Record{{Seq: 0}}, nil
		}
	}
	return j.Journal.Replay(ctx, id)
}

func TestFailedResumeOrphanReceiptRecovery(t *testing.T) {
	j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal()}
	b, req := resumeFixture(t, j)
	appendResumeIntent(t, j, req)
	j.appendFailures = 3
	if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
		t.Fatal("unpersisted receipt resolved intent", reply)
	}
	if j.appendCalls != 3 {
		t.Fatal("append retry not bounded", j.appendCalls)
	}
	if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" {
		t.Fatal("later request failed to recover receipt", reply)
	}
	calls := j.appendCalls
	if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" || j.appendCalls != calls {
		t.Fatal("receipt replay was not idempotent", reply)
	}
	conflict := req
	conflict.Seq++
	if reply := b.admitResume(context.Background(), conflict); reply.Error != "resume identity conflict" {
		t.Fatal(reply)
	}
	if b.runner.IsActive("run") {
		t.Fatal("orphan re-executed")
	}
}

func TestFailedResumeReceiptCursorAndRetries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		invalid  bool
		status   string
		calls    int
	}{
		{"transient", 1, false, "completed", 2},
		{"exhausted", 3, false, "ambiguous", 3},
		{"invalid cursor", 0, true, "ambiguous", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal()}
			b, req := resumeFixture(t, j)
			j.replayFaults, j.replayFailures, j.invalidCursor = true, tc.failures, tc.invalid
			j.appendFailures = 1
			done := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "completed", Outcome: &client.Outcome{Version: 1}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Receipt persistence is independent of request cancellation.
			if err := saveResumeReceipt(ctx, b.runner, &done); err != nil {
				t.Fatal(err)
			}
			if done.Status != tc.status || j.replayCalls != tc.calls || j.appendCalls != 2 {
				t.Fatal(done, j.replayCalls, j.appendCalls)
			}
			if done.Status == "completed" && done.Seq <= 0 {
				t.Fatal("completed without cursor", done)
			}
			if done.Status == "ambiguous" && (done.Outcome != nil || done.Seq != 0) {
				t.Fatal("ambiguous outcome retained", done)
			}
			recs, _ := j.Journal.Replay(context.Background(), "run")
			var saved reportprotocol.FailedResumeReply
			if err := json.Unmarshal(recs[len(recs)-1].Payload, &saved); err != nil || saved.Status != tc.status {
				t.Fatal(saved, err)
			}
		})
	}
}

func TestFailedResumeWorkerReceiptFailureBecomesRecoverableOrphan(t *testing.T) {
	j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal(), appendFailures: 3}
	b, req := resumeFixture(t, j)
	if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
		t.Fatal(reply)
	}
	b.resumeWG.Wait()
	b.submitMu.Lock()
	inFlight := len(b.resumeInFlight)
	b.submitMu.Unlock()
	if inFlight != 0 || j.appendCalls != 3 {
		t.Fatal("worker did not release in-flight identity after bounded persistence attempts", inFlight, j.appendCalls)
	}
	if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" {
		t.Fatal("failed worker receipt could not be recovered", reply)
	}
}

func TestResumeJournalRetryHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	err := retryResumeJournal(ctx, "test", &reportprotocol.FailedResumeReply{}, func() error {
		calls++
		return errors.New("transient failure")
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatal("retry did not stop at deadline", err, calls)
	}
}

func TestFailedResumeReconciliationFencesDelayedOriginal(t *testing.T) {
	for _, failReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt_failure_%t", failReceipt), func(t *testing.T) {
			j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal()}
			b, original := resumeFixture(t, j)
			executions := 0
			b.runner = runtime.NewRunner(j, func(context.Context, *runtime.Session) (runtime.Agent, error) {
				executions++
				return nil, errors.New("must not execute")
			})
			reconcile := original
			reconcile.Reconcile = true
			if failReceipt {
				j.appendFailures = 3
			}
			want := "ambiguous"
			if failReceipt {
				want = "pending"
			}
			if reply := b.admitResume(context.Background(), reconcile); reply.Status != want {
				t.Fatal(reply)
			}
			recs, _ := j.Journal.Replay(context.Background(), "run")
			var stored reportprotocol.FailedResume
			if err := json.Unmarshal(recs[2].Payload, &stored); err != nil || stored != original || stored.Reconcile {
				t.Fatal("reconciliation did not persist normalized intent", stored, err)
			}
			for _, req := range []reportprotocol.FailedResume{original, reconcile} {
				if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" {
					t.Fatal("delayed original/reconciliation did not recover final receipt", reply)
				}
			}
			b.resumeWG.Wait()
			if executions != 0 {
				t.Fatal("reconciliation executed command", executions)
			}
		})
	}
}

func TestFailedResumeReconciliationUnseenUnsafeSnapshotPending(t *testing.T) {
	for _, state := range []runtime.RunState{runtime.RunCompleted, runtime.RunRunning} {
		t.Run(string(state), func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, req := resumeFixture(t, j)
			if _, err := j.Append(context.Background(), runtime.Record{RunID: "run", Kind: runtime.RecordState, State: state}); err != nil {
				t.Fatal(err)
			}
			req.Reconcile = true
			if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
				t.Fatal(reply)
			}
			recs, _ := j.Replay(context.Background(), "run")
			for _, rec := range recs {
				if rec.ExtType == resumeIntentType || rec.ExtType == resumeReceiptType {
					t.Fatal("unsafe snapshot was fenced", rec)
				}
			}
		})
	}
}

func TestFailedResumeReconciliationExistingLocalInFlight(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	appendResumeIntent(t, j, req)
	b.resumeInFlight = map[reportprotocol.FailedResume]bool{req: true}
	req.Reconcile = true
	if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
		t.Fatal(reply)
	}
	unseen := req
	unseen.ID = "unseen"
	if reply := b.admitResume(context.Background(), unseen); reply.Status != "pending" {
		t.Fatal("unseen reconciliation bypassed local in-flight protection", reply)
	}
	recs, _ := j.Replay(context.Background(), "run")
	if recs[len(recs)-1].ExtType != resumeIntentType {
		t.Fatal("in-flight reconciliation finalized", recs)
	}
}

func TestFailedResumeReconciliationSnapshotFailurePending(t *testing.T) {
	j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal()}
	b, req := resumeFixture(t, j)
	j.snapshotFailure = true
	req.Reconcile = true
	if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
		t.Fatal(reply)
	}
	recs, _ := j.Journal.Replay(context.Background(), "run")
	if len(recs) != 2 {
		t.Fatal("failed snapshot fenced command", recs)
	}
}

func appendLegacyResumeReceipt(t *testing.T, j runtime.Journal, req reportprotocol.FailedResume, edit func(*reportprotocol.FailedResumeReply)) {
	t.Helper()
	appendResumeIntent(t, j, req)
	receipt := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "completed", Seq: req.Seq + 1,
		Outcome: &client.Outcome{Version: 1, TaskID: req.Target.TaskID, AgentID: req.Target.AgentID, RunID: req.Target.RunID, AttemptID: req.Target.AttemptID, State: runtime.RunFailed}}
	edit(&receipt)
	raw, _ := json.Marshal(receipt)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := j.Append(ctx, runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeReceiptType, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedResumeReconciliationUpgradesInvalidCompletedReceipt(t *testing.T) {
	for name, edit := range map[string]func(*reportprotocol.FailedResumeReply){
		"zero cursor":     func(v *reportprotocol.FailedResumeReply) { v.Seq = 0 },
		"equal cursor":    func(v *reportprotocol.FailedResumeReply) { v.Seq-- },
		"missing outcome": func(v *reportprotocol.FailedResumeReply) { v.Outcome = nil },
		"wrong version":   func(v *reportprotocol.FailedResumeReply) { v.Outcome.Version = 0 },
		"wrong task":      func(v *reportprotocol.FailedResumeReply) { v.Outcome.TaskID = "other" },
		"wrong agent":     func(v *reportprotocol.FailedResumeReply) { v.Outcome.AgentID = "other" },
		"wrong run":       func(v *reportprotocol.FailedResumeReply) { v.Outcome.RunID = "other" },
		"wrong attempt":   func(v *reportprotocol.FailedResumeReply) { v.Outcome.AttemptID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, req := resumeFixture(t, j)
			appendLegacyResumeReceipt(t, j, req, edit)
			if reply := b.admitResume(context.Background(), req); reply.Status != "completed" {
				t.Fatal("normal request changed legacy receipt", reply)
			}
			reconcile := req
			reconcile.Reconcile = true
			if reply := b.admitResume(context.Background(), reconcile); reply.Status != "ambiguous" || reply.Outcome != nil {
				t.Fatal(reply)
			}
			recs, _ := j.Replay(context.Background(), "run")
			var saved reportprotocol.FailedResumeReply
			if err := json.Unmarshal(recs[len(recs)-1].Payload, &saved); err != nil || saved.Status != "ambiguous" {
				t.Fatal("upgrade not durable", saved, err)
			}
			for range 3 {
				if reply := b.admitResume(context.Background(), req); reply.Status != "ambiguous" || reply.Seq != saved.Seq {
					t.Fatal("legacy receipt hides upgrade", reply)
				}
			}
			after, _ := j.Replay(context.Background(), "run")
			if len(after) != len(recs) || b.runner.IsActive("run") {
				t.Fatal("receipt replay executed or appended")
			}
		})
	}
}

func TestFailedResumeInvalidReceiptReconciliationProtection(t *testing.T) {
	for _, protection := range []string{"inflight", "snapshot failure", "not failed"} {
		t.Run(protection, func(t *testing.T) {
			j := &resumeFaultJournal{Journal: runtime.NewMemoryJournal()}
			b, req := resumeFixture(t, j)
			appendLegacyResumeReceipt(t, j, req, func(v *reportprotocol.FailedResumeReply) { v.Seq = 0 })
			switch protection {
			case "inflight":
				b.resumeInFlight = map[reportprotocol.FailedResume]bool{req: true}
			case "snapshot failure":
				j.snapshotFailure = true
			case "not failed":
				_, _ = j.Append(context.Background(), runtime.Record{RunID: "run", Kind: runtime.RecordState, State: runtime.RunCompleted})
			}
			before, _ := j.Journal.Replay(context.Background(), "run")
			req.Reconcile = true
			if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
				t.Fatal(reply)
			}
			after, _ := j.Journal.Replay(context.Background(), "run")
			if len(after) != len(before) {
				t.Fatal("unsafe legacy receipt upgraded")
			}
		})
	}
}

func TestFailedResumeReceiptIdentityAndValidCompletionPreserved(t *testing.T) {
	for _, change := range []string{"valid", "target", "command"} {
		t.Run(change, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, req := resumeFixture(t, j)
			appendLegacyResumeReceipt(t, j, req, func(v *reportprotocol.FailedResumeReply) {
				if change == "target" {
					v.Target.TaskID = "other"
				}
			})
			if change == "command" {
				req.Seq++
			}
			req.Reconcile = true
			reply := b.admitResume(context.Background(), req)
			if change == "valid" {
				if reply.Status != "completed" {
					t.Fatal(reply)
				}
			} else if reply.Status != "rejected" || reply.Error != "resume identity conflict" {
				t.Fatal(reply)
			}
			recs, _ := j.Replay(context.Background(), "run")
			if len(recs) != 4 {
				t.Fatal("valid/conflicting receipt overwritten")
			}
		})
	}
}

func TestMismatchedReceiptCannotClearEarlierInFlightIntent(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	raw, _ := json.Marshal(req)
	_, _ = j.Append(t.Context(), runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: raw})
	wrong := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "ambiguous"}
	wrong.Target.AttemptID = "other"
	raw, _ = json.Marshal(wrong)
	seq, _ := j.Append(t.Context(), runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeReceiptType, Payload: raw})
	b.resumeInFlight = map[reportprotocol.FailedResume]bool{req: true}
	next := req
	next.ID = "next"
	next.Seq = seq
	reply := b.admitResume(t.Context(), next)
	if reply.Status != "rejected" || reply.Error != "earlier resume remains unconfirmed" {
		t.Fatalf("unsafe new command: %+v", reply)
	}
	if b.runner.IsActive(req.Target.RunID) {
		t.Fatal("new execution started")
	}
}

// failingAgent fails every prompt like a provider overload would.
type failingAgent struct{}

func (failingAgent) PromptResult(context.Context, string) (*kit.TurnResult, error) {
	return nil, errors.New("stream error: Our servers are currently overloaded")
}
func (failingAgent) InjectSteer(string) {}
func (failingAgent) Close() error       { return nil }

// A continuation that executes and fails again is a confirmed failed outcome,
// not an ambiguous execution; otherwise automatic resume stops needlessly.
func TestFailedResumeRepeatedFailureIsConfirmed(t *testing.T) {
	for name, factory := range map[string]runtime.AgentFactory{
		"turn failure": func(context.Context, *runtime.Session) (runtime.Agent, error) { return failingAgent{}, nil },
		"agent construction failure": func(context.Context, *runtime.Session) (runtime.Agent, error) {
			return nil, errors.New("agent construction failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, req := resumeFixture(t, j)
			b.runner = runtime.NewRunner(j, factory)
			if reply := b.admitResume(context.Background(), req); reply.Status != "pending" {
				t.Fatal(reply)
			}
			b.resumeWG.Wait()
			reply := b.admitResume(context.Background(), req)
			if reply.Status != "completed" || !validCompletedResumeReceipt(req, reply) {
				t.Fatalf("receipt = %+v, want completed", reply)
			}
			if reply.Outcome.State != runtime.RunFailed || reply.Outcome.Error == "" {
				t.Fatalf("outcome = %+v, want failed with error", reply.Outcome)
			}
		})
	}
}

// Without proof that a new turn ran, a nil result stays ambiguous.
func TestConfirmedResumeResultRequiresNewTurn(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, req := resumeFixture(t, j)
	if got := confirmedResumeResult(context.Background(), b.runner, req, nil, errors.New("busy")); got != nil {
		t.Fatalf("unchanged failed turn confirmed: %+v", got)
	}
	running := &runtime.Run{ID: "run", State: runtime.RunRunning, Err: errors.New("completion write")}
	if got := confirmedResumeResult(context.Background(), b.runner, req, running, running.Err); got != nil {
		t.Fatalf("non-failed result confirmed: %+v", got)
	}
}
