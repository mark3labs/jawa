package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

// Reserve through the real eligibility/status path, without background workers.
func reconcileResumeFixture(t *testing.T, planning, automatic bool) (*Workflow, *nats.Conn, Card, reportprotocol.FailedResume, string) {
	t.Helper()
	s, nc := workflowFixture(t)
	cli, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: cli, resumeStaleAfter: time.Second}
	card := workflowCard(t, s)
	lane := "Building"
	if planning {
		lane = "Planning"
	}
	if err = w.MoveCard(card.ID, lane, 0); err != nil {
		t.Fatal(err)
	}
	var task string
	if planning {
		task = planningDetails(t, w, card.ID).Turns[0].ID
	} else {
		a, e := w.CardResult(card.ID)
		if e != nil {
			t.Fatal(e)
		}
		task = a.TaskID
	}
	out := client.Outcome{Version: 1, TaskID: task, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "original execution failure"}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: task, AgentID: out.AgentID, RunID: out.RunID, AttemptID: out.AttemptID}
	sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
		raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 20})
		_ = m.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if automatic {
		if err = w.discoverAutoResumes(now); err != nil {
			t.Fatal(err)
		}
		w.autoResumePass(now.Add(5 * time.Second))
	} else if err = w.reserveFailedResume(card.ID, planning, "", false, now); err != nil {
		t.Fatal(err)
	}
	if err = sub.Unsubscribe(); err != nil {
		t.Fatal(err)
	}
	if err = nc.Flush(); err != nil {
		t.Fatal(err)
	}
	var raw, previous string
	if err = s.db.QueryRow(`SELECT request_json,previous_outcome FROM failed_resumes WHERE task_id=? AND status='pending'`, task).Scan(&raw, &previous); err != nil {
		t.Fatal(err)
	}
	var req reportprotocol.FailedResume
	if err = json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	return w, nc, card, req, previous
}

func reconcileResumeState(t *testing.T, w *Workflow, req reportprotocol.FailedResume, planning bool, wantStatus, wantState, wantOutcome string) string {
	t.Helper()
	var status, state, outcome, notice string
	if err := w.s.db.QueryRow(`SELECT status FROM failed_resumes WHERE id=?`, req.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	table, key := "workflow_attempts", "task_id"
	if planning {
		table, key = "planning_turns", "id"
	}
	if err := w.s.db.QueryRow(`SELECT state,outcome_json,error FROM `+table+` WHERE `+key+`=?`, req.Target.TaskID).Scan(&state, &outcome, &notice); err != nil {
		t.Fatal(err)
	}
	if status != wantStatus || state != wantState || outcome != wantOutcome {
		t.Fatalf("status/state/outcome = %q/%q/%q, want %q/%q/%q", status, state, outcome, wantStatus, wantState, wantOutcome)
	}
	if planning {
		var sessionState string
		if err := w.s.db.QueryRow(`SELECT p.state FROM planning_sessions p JOIN planning_turns t ON t.session_id=p.id WHERE t.id=?`, req.Target.TaskID).Scan(&sessionState); err != nil {
			t.Fatal(err)
		}
		if sessionState != wantState {
			t.Fatalf("session state = %q, want %q", sessionState, wantState)
		}
	} else {
		var runState string
		if err := w.s.db.QueryRow(`SELECT run_state FROM workflow_attempts WHERE task_id=?`, req.Target.TaskID).Scan(&runState); err != nil {
			t.Fatal(err)
		}
		if runState != wantState {
			t.Fatalf("run state = %q, want %q", runState, wantState)
		}
	}
	return notice
}

func TestResumeReconcileAmbiguousRestoresFailureAndStopsAuto(t *testing.T) {
	for _, planning := range []bool{false, true} {
		mode := "build"
		if planning {
			mode = "planning"
		}
		for _, automatic := range []bool{false, true} {
			kind := "manual"
			if automatic {
				kind = "auto"
			}
			t.Run(mode+"/"+kind, func(t *testing.T) {
				w, _, _, req, previous := reconcileResumeFixture(t, planning, automatic)
				reply := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "ambiguous", Error: "host cannot confirm continuation"}
				if err := w.finishFailedResume(req.ID, reply); err != nil {
					t.Fatal(err)
				}
				notice := reconcileResumeState(t, w, req, planning, "ambiguous", "failed", previous)
				if notice != "Resume could not be confirmed: "+reply.Error {
					t.Fatalf("notice = %q", notice)
				}
				if err := w.finishFailedResume(req.ID, reply); err != nil {
					t.Fatal(err)
				}
				if automatic {
					w.autoResumePass(time.Now().Add(time.Hour))
					var state, token string
					if err := w.s.db.QueryRow(`SELECT state,failure_token FROM auto_resumes WHERE task_id=?`, req.Target.TaskID).Scan(&state, &token); err != nil {
						t.Fatal(err)
					}
					if state != "stopped" || token != resumeToken(Attempt{TaskID: req.Target.TaskID, EventSeq: req.Seq, OutcomeJSON: previous}) {
						t.Fatalf("automatic schedule = %q/%q", state, token)
					}
					var count int
					if err := w.s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE task_id=?`, req.Target.TaskID).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 1 {
						t.Fatalf("stopped schedule issued %d commands", count)
					}
				}
			})
		}
	}
}

func TestResumeReconcileStaleDispatch(t *testing.T) {
	for _, planning := range []bool{false, true} {
		mode := "build"
		if planning {
			mode = "planning"
		}
		for _, scenario := range []string{"inactive-failed", "active", "unreachable", "invalid-cursor"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				w, nc, _, req, previous := reconcileResumeFixture(t, planning, false)
				if _, err := w.s.db.Exec(`UPDATE failed_resumes SET created_at=? WHERE id=?`, time.Now().Add(-time.Hour).UnixMilli(), req.ID); err != nil {
					t.Fatal(err)
				}
				var logs bytes.Buffer
				w.logger = log.NewWithOptions(&logs, log.Options{Level: log.DebugLevel})
				queries := make(chan struct{}, 2)
				if scenario != "unreachable" {
					sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
						queries <- struct{}{}
						raw, _ := json.Marshal(client.Status{Version: 1, Target: req.Target, State: runtime.RunFailed, Active: scenario == "active", TurnID: req.TurnID, Seq: req.Seq})
						_ = m.Respond(raw)
					})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = sub.Unsubscribe() })
				}
				commands := make(chan reportprotocol.FailedResume, 2)
				// The first invalid receipt must roll back; a later fence resolves the same ID.
				attempts := 0
				sub, err := nc.Subscribe(reportprotocol.ResumeSubject("bonnie", req.Target.AgentID), func(m *nats.Msg) {
					var got reportprotocol.FailedResume
					if json.Unmarshal(m.Data, &got) != nil {
						return
					}
					attempts++
					commands <- got
					reply := reportprotocol.FailedResumeReply{Version: 1, ID: got.ID, Target: got.Target, Status: "pending"}
					if scenario == "inactive-failed" || (scenario == "invalid-cursor" && attempts > 1) {
						reply.Status, reply.Error = "ambiguous", "idle command fenced"
					} else if scenario == "invalid-cursor" {
						var out client.Outcome
						_ = json.Unmarshal([]byte(previous), &out)
						reply.Status, reply.Seq, reply.Outcome = "completed", req.Seq, &out
					}
					raw, _ := json.Marshal(reply)
					_ = m.Respond(raw)
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = sub.Unsubscribe() })
				if err = nc.Flush(); err != nil {
					t.Fatal(err)
				}
				w.dispatchFailedResumes(nc)
				// Read only after dispatch returns, so callback logging/state is settled.
				got := <-commands
				wantReconcile := scenario == "inactive-failed" || scenario == "invalid-cursor"
				expected := req
				expected.Reconcile = wantReconcile
				if got != expected {
					t.Fatalf("dispatched command = %+v, want %+v", got, expected)
				}
				if scenario != "unreachable" {
					select {
					case <-queries:
					default:
						t.Fatal("stale command did not query owner")
					}
				}
				if scenario == "inactive-failed" {
					reconcileResumeState(t, w, req, planning, "ambiguous", "failed", previous)
				} else {
					notice := reconcileResumeState(t, w, req, planning, "pending", "running", "")
					if notice != resumePendingNotice {
						t.Fatalf("pending notice = %q", notice)
					}
				}
				if scenario == "invalid-cursor" {
					if !strings.Contains(logs.String(), "Failed to apply resume receipt") || !strings.Contains(logs.String(), req.ID) {
						t.Fatalf("missing receipt error log: %s", logs.String())
					}
					w.dispatchFailedResumes(nc)
					later := <-commands
					if later != expected {
						t.Fatalf("reconciliation changed command identity: %+v", later)
					}
					reconcileResumeState(t, w, req, planning, "ambiguous", "failed", previous)
				}
				var persisted string
				if err = w.s.db.QueryRow(`SELECT request_json FROM failed_resumes WHERE id=?`, req.ID).Scan(&persisted); err != nil {
					t.Fatal(err)
				}
				var durable reportprotocol.FailedResume
				if err = json.Unmarshal([]byte(persisted), &durable); err != nil {
					t.Fatal(err)
				}
				if durable != req {
					t.Fatalf("dispatch mutated durable command: %+v", durable)
				}
			})
		}
	}
}

func TestResumeStaleDuration(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 5 * time.Minute, false}, {"30s", 30 * time.Second, false}, {"1h30m", 90 * time.Minute, false}, {"1ms", time.Millisecond, false},
		{"0s", 0, true}, {"-1s", 0, true}, {"garbage", 0, true}, {"30", 0, true}, {" 1s ", 0, true}, {"999999999999999999999h", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("JAWA_RESUME_STALE_AFTER", tc.value)
			got, err := resumeStaleDuration()
			if got != tc.want || (err != nil) != tc.invalid {
				t.Fatalf("resumeStaleDuration() = %v, %v; want %v, invalid=%v", got, err, tc.want, tc.invalid)
			}
			if tc.invalid && !strings.Contains(err.Error(), "JAWA_RESUME_STALE_AFTER must be a positive duration") {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
