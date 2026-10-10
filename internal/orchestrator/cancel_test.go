package orchestrator

import (
	"context"
	"encoding/json"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestCancelCardRequiresRemoteClient(t *testing.T) {
	s, _ := workflowFixture(t)
	if err := migrateWorkflow(s); err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: context.Background()}
	c := workflowCard(t, s)
	if err := w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	if err := w.CancelCard(context.Background(), c.ID); err == nil {
		t.Fatal("expected cancellation without a client to fail safely")
	}
	a, err := w.CardResult(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.State == "failed" || a.OutcomeJSON != "" {
		t.Fatalf("cancellation changed execution state: %+v", a)
	}
}

func TestCancelCardNATSRequestReply(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		state    string
		wantErr  bool
		inactive bool
	}{
		{name: "requested waits for event"},
		{name: "stale command reply", mode: "stale", wantErr: true},
		{name: "no query responder", mode: "no-query", wantErr: true},
		{name: "missing target", mode: "missing", wantErr: true},
		{name: "parked waiting", state: "waiting", inactive: true},
		{name: "inactive interrupted", state: "interrupted", inactive: true},
		{name: "inactive saved running", inactive: true},
		{name: "semantic stale", mode: "stale-status", wantErr: true},
		{name: "not active", mode: "not_active", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, nc := workflowFixture(t)
			if err := migrateWorkflow(s); err != nil {
				t.Fatal(err)
			}
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
			target := client.Target{TaskID: a.TaskID, AgentID: "cancel-agent", RunID: "cancel-run", AttemptID: "cancel-attempt"}
			state := runtime.RunRunning
			if tc.state != "" {
				state = runtime.RunState(tc.state)
			}
			ev := client.StatusEvent{Version: 1, Target: target, Type: "run_state", EventID: "running-target", Seq: 1, State: state}
			if err := w.event(t.Context(), ev); err != nil {
				t.Fatal(err)
			}
			if tc.mode != "no-query" {
				queryStatus := client.Status{Version: 1, Target: target, State: state, Active: !tc.inactive, TurnID: "turn-exact"}
				if tc.mode == "missing" {
					queryStatus.RunID = "other-run"
				}
				if err := replyJSON(nc, "bonnie.queries.cancel-agent", queryStatus); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mode != "no-query" && tc.mode != "missing" {
				_, err := nc.Subscribe("bonnie.commands.cancel-agent", func(m *nats.Msg) {
					var req struct {
						Version int `json:"version"`
						client.Target
						TurnID string `json:"turn_id"`
					}
					if json.Unmarshal(m.Data, &req) != nil {
						return
					}
					if req.Target != target || req.TurnID != "turn-exact" {
						t.Errorf("unexpected cancel request: %+v", req)
					}
					reply := client.Status{Version: 1, Target: target, State: state, TurnID: req.TurnID, CancelStatus: "requested"}
					if tc.mode == "stale" {
						reply.TurnID = "old-turn"
					}
					if tc.mode == "stale-status" {
						reply.CancelStatus = "stale"
					}
					if tc.mode == "not_active" {
						reply.CancelStatus = "not_active"
					}
					data, _ := json.Marshal(reply)
					_ = m.Respond(data)
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := nc.Flush(); err != nil {
				t.Fatal(err)
			}
			err = w.CancelCard(t.Context(), c.ID)
			if tc.wantErr && err == nil {
				t.Fatal("expected cancellation to fail")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CancelCard: %v", err)
			}
			if err = w.ResetCard(c.ID); err == nil {
				t.Fatal("reset accepted before cancelled event")
			}
			if err = w.DeleteCard(c.ID); err == nil {
				t.Fatal("delete accepted before cancelled event")
			}
			if !tc.wantErr {
				cancelled := ev
				cancelled.EventID = "cancelled-target"
				cancelled.Seq = 2
				cancelled.State = runtime.RunCancelled
				if err := w.event(t.Context(), cancelled); err != nil {
					t.Fatal(err)
				}
				if err := w.ResetCard(c.ID); err != nil {
					t.Fatalf("reset after cancellation event: %v", err)
				}
			}
		})
	}
}

func replyJSON(nc *nats.Conn, subject string, status client.Status) error {
	_, err := nc.Subscribe(subject, func(m *nats.Msg) { data, _ := json.Marshal(status); _ = m.Respond(data) })
	if err == nil {
		err = nc.Flush()
	}
	return err
}
