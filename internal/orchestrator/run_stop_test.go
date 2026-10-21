package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/presence"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestStopRunTurnAuditAndStaleTurn(t *testing.T) {
	for _, mode := range []string{"requested", "uncertain", "stale"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			attempt := pinnedDetailAttempt(t, a)
			subject := "bonnie.api.agent." + attempt.AgentID
			if err := a.workflow.presence.Register(t.Context(), presence.Record{Identity: presence.Identity{Agent: attempt.AgentID, Instance: "one"}, State: presence.Ready, Labels: map[string]string{"jawa.api_subject": subject}}); err != nil {
				t.Fatal(err)
			}
			nc := a.workflow.runAPI.conn
			// Serve the HTTP-over-NATS framing with deterministic snapshots and cancel
			// receipts so both audit outcomes are independent of model timing.
			sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
				var req struct {
					ID     string `json:"request_id"`
					Method string `json:"method"`
					Path   string `json:"path"`
				}
				if e := json.Unmarshal(msg.Data, &req); e != nil {
					t.Error(e)
					return
				}
				var body any = bonnieclient.Snapshot{RunID: attempt.RunID, TurnID: "turn-current", State: runtime.RunRunning}
				if req.Method == "POST" {
					if mode == "stale" {
						t.Error("stale turn sent cancellation")
					}
					var pending int
					if e := a.s.db.QueryRow(`SELECT count(*) FROM workflow_cancel_requests WHERE task_id=? AND status='pending'`, attempt.TaskID).Scan(&pending); e != nil || pending != 1 {
						t.Error("pending audit missing before send", e, pending)
					}
					turn := "turn-current"
					if mode == "uncertain" {
						turn = "other-turn"
					}
					body = runtime.CancelResult{RunID: attempt.RunID, TurnID: turn, Status: runtime.CancelRequested}
				}
				data, _ := json.Marshal(body)
				for i, kind := range []string{"headers", "data", "end"} {
					frame := map[string]any{"version": 1, "request_id": req.ID, "sequence": i, "kind": kind}
					if kind == "headers" {
						frame["status"] = 200
						frame["lease_ms"] = 30000
						frame["renew_ms"] = 10000
						frame["window"] = 8
					}
					if kind == "data" {
						frame["data"] = data
					}
					raw, _ := json.Marshal(frame)
					if e := msg.Respond(raw); e != nil {
						t.Error(e)
						return
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			if err = nc.Flush(); err != nil {
				t.Fatal(err)
			}
			turn := "turn-current"
			if mode == "stale" {
				turn = "turn-old"
			}
			outcome, err := a.workflow.StopRunTurn(t.Context(), attempt, turn)
			switch mode {
			case "stale":
				if !errors.Is(err, errStopTurnChanged) {
					t.Fatalf("want stale: %v", err)
				}
			case "uncertain":
				if !errors.Is(err, errStopOutcomeUnknown) {
					t.Fatalf("want unknown: %v", err)
				}
			default:
				if err != nil || outcome.Attempt.ID != attempt.ID {
					t.Fatal(outcome, err)
				}
			}
			rows, err := a.s.db.Query(`SELECT status,turn_id FROM workflow_cancel_requests WHERE task_id=? ORDER BY id`, attempt.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var statuses []string
			for rows.Next() {
				var state, auditedTurn string
				if err = rows.Scan(&state, &auditedTurn); err != nil {
					t.Fatal(err)
				}
				if auditedTurn != "turn-current" {
					t.Fatal(auditedTurn)
				}
				statuses = append(statuses, state)
			}
			if err = rows.Err(); err != nil {
				t.Fatal(err)
			}
			want := "pending," + mode
			if mode == "stale" {
				want = ""
			}
			if strings.Join(statuses, ",") != want {
				t.Fatal(statuses, want)
			}
		})
	}
}
