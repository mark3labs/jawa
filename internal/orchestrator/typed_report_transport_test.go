package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"jawa/internal/reportprotocol"
)

func TestTypedReportNATSCommitAckAndLostReplyReplay(t *testing.T) {
	t.Parallel()
	s, nc := workflowFixture(t)
	c := workflowCard(t, s)
	w := &Workflow{s: s, ctx: context.Background(), observeReport: fakeReportObserver}
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	w.ctx, w.cancel = ctx, cancel
	if err := w.subscribeReports(nc); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	a, _ := w.CardResult(c.ID)
	r := reportRequest(t, a.TaskID, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: 3})
	transport := reportprotocol.Client{Conn: nc, Root: "bonnie"}
	// Rejected transactions must never produce an accepted receipt on the wire.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_receipt BEFORE INSERT ON typed_reports BEGIN SELECT RAISE(ABORT,'storage fault'); END`); err != nil {
		t.Fatal(err)
	}
	ack, err := transport.Submit(t.Context(), r)
	if err == nil || ack.Accepted {
		t.Fatal("failed commit acknowledged", ack, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM typed_reports`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER reject_receipt`); err != nil {
		t.Fatal(err)
	}
	// Fire-and-forget simulates a sender losing its reply after receiver commit.
	data, _ := json.Marshal(r)
	if err = nc.Publish(reportprotocol.Subject("bonnie"), data); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		var n int
		return s.db.QueryRow(`SELECT count(*) FROM typed_reports WHERE task_id=?`, r.TaskID).Scan(&n) == nil && n == 1
	})
	ack, err = transport.Submit(t.Context(), r)
	if err != nil || !ack.Accepted || !ack.Duplicate {
		t.Fatal(ack, err)
	}
	a, _ = w.CardResult(c.ID)
	if a.PRURL != "https://github.com/example/repo/pull/3" || a.OutcomeJSON != string(data) {
		t.Fatal("ACK lacks committed transition", a)
	}
	// Wire decoding rejects unknown fields instead of allowing model identity or
	// alternate payload contracts to slip through JSON's permissive defaults.
	malformed := append(data[:len(data)-1], []byte(`,"model_identity":"spoof"}`)...)
	reply, err := nc.Request(reportprotocol.Subject("bonnie"), malformed, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(reply.Data, &ack); err != nil || ack.Accepted || ack.Error == "" {
		t.Fatal(ack, err)
	}
}

func TestTypedReportReceiptSurvivesDatabaseReopenAndCardDeletion(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "board.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, s)
	w := &Workflow{s: s, ctx: context.Background(), observeReport: fakeReportObserver}
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(c.ID)
	r := reportRequest(t, a.TaskID, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: "cannot proceed"})
	if ack, err := w.acceptReport(t.Context(), r); err != nil || !ack.Accepted {
		t.Fatal(ack, err)
	}
	if err = w.DeleteCard(c.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	w = &Workflow{s: s, ctx: context.Background()}
	if ack, err := w.acceptReport(t.Context(), r); err != nil || !ack.Accepted || !ack.Duplicate {
		t.Fatal("durable deleted task receipt lost", ack, err)
	}
}
