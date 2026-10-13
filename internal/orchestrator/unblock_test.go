package orchestrator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

func blockedFixture(t *testing.T, repair bool) (*Workflow, Card, Attempt) {
	t.Helper()
	s, nc := workflowFixture(t)
	c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: c, resumeConn: nc}
	card := workflowCard(t, s)
	if repair {
		repairDone(t, w, card)
		err = w.FixCard(card.ID, "repair")
	} else {
		err = w.MoveCard(card.ID, "Building", 0)
	}
	if err != nil {
		t.Fatal(err)
	}
	a, err := w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}
	if err = w.event(t.Context(), client.StatusEvent{Version: 1, Target: target, EventID: "start", Seq: 1, State: runtime.RunRunning}); err != nil {
		t.Fatal(err)
	}
	w.observeReport = fakeReportObserver
	r, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", TurnID: "turn", ToolCallID: "blocker"}, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "operator needed"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.acceptReport(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
		raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunCompleted, TurnID: "turn", Seq: 20})
		_ = m.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	a, err = w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	return w, card, a
}

func TestUnblockReservationAndRestoration(t *testing.T) {
	for _, status := range []string{"rejected", "ambiguous"} {
		t.Run(status, func(t *testing.T) {
			w, card, a := blockedFixture(t, false)
			token := unblockToken(a, "Building")
			for _, prompt := range []string{"", "  ", strings.Repeat("é", 1025), string([]byte{0xff})} {
				if err := w.Unblock(card.ID, token, prompt); err == nil {
					t.Fatal("invalid prompt accepted")
				}
			}
			if err := w.Unblock(card.ID, "stale", "resume"); err == nil {
				t.Fatal("stale accepted")
			}
			if err := w.Unblock(card.ID, token, "  finish within scope  "); err != nil {
				t.Fatal(err)
			}
			after, _ := w.CardResult(card.ID)
			if after.ID != a.ID || after.TaskID != a.TaskID || after.RunID != a.RunID || after.Branch != a.Branch || after.ReportGeneration != 1 {
				t.Fatal(after)
			}
			for _, action := range []func(string) error{w.Retry, w.ResetCard, w.DeleteCard, w.ResumeFailed} {
				if err := action(card.ID); err == nil {
					t.Fatal("pending command raced")
				}
			}
			if err := w.Unblock(card.ID, token, "resume"); err == nil {
				t.Fatal("duplicate reserved")
			}
			var id, raw string
			if err := w.s.db.QueryRow(`SELECT id,request_json FROM failed_resumes WHERE task_id=?`, a.TaskID).Scan(&id, &raw); err != nil {
				t.Fatal(err)
			}
			var req reportprotocol.FailedResume
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				t.Fatal(err)
			}
			if req.Prompt != "finish within scope" || req.BlockerReportID == "" {
				t.Fatal(req)
			}
			if err := w.finishFailedResume(id, reportprotocol.FailedResumeReply{Version: 1, ID: id, Target: req.Target, Status: status, Error: "test rejection"}); err != nil {
				t.Fatal(err)
			}
			restored, _ := w.CardResult(card.ID)
			if restored.State != a.State || restored.RunState != a.RunState || restored.Error != a.Error || restored.Result != a.Result || restored.OutcomeJSON != a.OutcomeJSON || restored.ReportGeneration != 0 {
				t.Fatal("snapshot not restored", restored)
			}
			if err := w.Unblock(card.ID, unblockToken(restored, "Building"), "try explicitly again"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForceReadyRepairAuditAndIdempotence(t *testing.T) {
	w, card, a := blockedFixture(t, true)
	if a.PRURL != "" || a.RepairPRURL == "" {
		t.Fatal(a)
	}
	token := unblockToken(a, "Fix")
	if err := w.Unblock(card.ID, token, "resume"); err == nil {
		t.Fatal("repair PR resumed through no-PR action")
	}
	if err := w.ForceReady(card.ID, token, "admin", false); err == nil {
		t.Fatal("missing acknowledgement")
	}
	if err := w.ForceReady(card.ID, "stale", "admin", true); err == nil {
		t.Fatal("stale accepted")
	}
	w.SetVerifier(func(context.Context, Attempt) (bool, error) {
		t.Error("verifier invoked by force-ready")
		return false, nil
	})
	if err := w.ForceReady(card.ID, token, "operator", true); err != nil {
		t.Fatal(err)
	}
	if err := w.ForceReady(card.ID, token, "operator", true); err != nil {
		t.Fatal(err)
	}
	ready, err := w.CardResult(card.ID)
	if err != nil || !ready.Ready || !ready.ManualReady || ready.PRURL != a.RepairPRURL || ready.Result != a.Result || ready.OutcomeJSON != a.OutcomeJSON || runLabel(ready) != "Ready (manual override)" {
		t.Fatal(ready, err)
	}
	assertActionLane(t, w, card.ProjectID, "Done", card.ID)
	var count int
	var operator, prior string
	if err = w.s.db.QueryRow(`SELECT count(*),operator,prior_snapshot FROM workflow_ready_overrides WHERE attempt_id=?`, a.ID).Scan(&count, &operator, &prior); err != nil || count != 1 || operator != "operator" || !strings.Contains(prior, "operator needed") {
		t.Fatal(count, operator, prior, err)
	}
	w.verifyPending()
	if err = w.MarkPRMerged(t.Context(), card.ID, ready.ID, ready.PRURL); err != nil {
		t.Fatal(err)
	}
	if err = w.FixCard(card.ID, "next repair"); err != nil {
		t.Fatal(err)
	}
}

func TestUnblockRejectsUnsafeOwnership(t *testing.T) {
	for name, statement := range map[string]string{
		"conflict":     `INSERT INTO workflow_conflicts(task_id,agent_id,run_id,remote_attempt_id,outcome_json,received_at,reason) VALUES(?,'other','other','other','{}',1,'conflict')`,
		"cancellation": `INSERT INTO workflow_cancel_requests(task_id,status) VALUES(?,'uncertain')`,
	} {
		t.Run(name, func(t *testing.T) {
			w, card, a := blockedFixture(t, false)
			if _, err := w.s.db.Exec(statement, a.TaskID); err != nil {
				t.Fatal(err)
			}
			if err := w.Unblock(card.ID, unblockToken(a, "Building"), "resume"); err == nil {
				t.Fatal("unsafe reservation")
			}
		})
	}
}
