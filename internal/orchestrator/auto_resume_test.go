package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

func TestAutoResumeFiveGenerations(t *testing.T) {
	s, nc := workflowFixture(t)
	c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: c}
	card := workflowCard(t, s)
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "runtime failure"}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}
	var mu sync.Mutex
	seq := 10
	queries := 0
	sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
		mu.Lock()
		defer mu.Unlock()
		queries++
		raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: seq})
		_ = m.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	_ = nc.Flush()
	now := time.Unix(1700000000, 0)
	for i := range 5 {
		if err = w.discoverAutoResumes(now); err != nil {
			t.Fatal(err)
		}
		var due int64
		if err = s.db.QueryRow(`SELECT due_at FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&due); err != nil || due != now.Add(autoResumeDelay(i)).UnixMilli() {
			t.Fatal(i, due, err)
		}
		if err = w.discoverAutoResumes(now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		var unchanged int64
		_ = s.db.QueryRow(`SELECT due_at FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&unchanged)
		if unchanged != due {
			t.Fatal("deadline postponed")
		}
		w.autoResumePass(time.UnixMilli(due - 1))
		var count int
		_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE automatic=1`).Scan(&count)
		if count != i {
			t.Fatal("early reservation", count)
		}
		// Reconstructed workflow uses the same persisted schedule.
		w = &Workflow{s: s, ctx: t.Context(), client: c}
		w.autoResumePass(time.UnixMilli(due))
		_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE automatic=1`).Scan(&count)
		if count != i+1 {
			t.Fatal("missing reservation", count)
		}
		var raw string
		_ = s.db.QueryRow(`SELECT request_json FROM failed_resumes WHERE status='pending'`).Scan(&raw)
		var req reportprotocol.FailedResume
		_ = json.Unmarshal([]byte(raw), &req)
		receipt := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: target, Status: "completed", Seq: req.Seq + 1, Outcome: &out}
		if err = w.finishFailedResume(req.ID, receipt); err != nil {
			t.Fatal(err)
		}
		if err = w.finishFailedResume(req.ID, receipt); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		seq += 2
		mu.Unlock()
		now = time.UnixMilli(due).Add(time.Second)
	}
	w.autoResumePass(now.Add(time.Hour))
	var state string
	_ = s.db.QueryRow(`SELECT state FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&state)
	if state != "exhausted" {
		t.Fatal(state)
	}
	mu.Lock()
	gotQueries := queries
	mu.Unlock()
	if gotQueries != 5 {
		t.Fatal("extra status calls", gotQueries)
	}
	if err = w.Retry(card.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := w.CardResult(card.ID)
	if fresh.TaskID == a.TaskID {
		t.Fatal("Retry did not create new budget identity")
	}
	out.TaskID = fresh.TaskID
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if err = w.discoverAutoResumes(now); err != nil {
		t.Fatal(err)
	}
	var freshDue int64
	if err = s.db.QueryRow(`SELECT due_at FROM auto_resumes WHERE task_id=?`, fresh.TaskID).Scan(&freshDue); err != nil || freshDue != now.Add(5*time.Second).UnixMilli() {
		t.Fatal("fresh budget not reset", freshDue, err)
	}
}

func TestAutoResumeToggleAndMissingOwner(t *testing.T) {
	s, nc := workflowFixture(t)
	c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: c}
	enabled, err := autoResumeEnabled(s.db)
	if err != nil || !enabled {
		t.Fatal(enabled, err)
	}
	card := workflowCard(t, s)
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "absent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	w.autoResumePass(now)
	if err = s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
		t.Fatal(err)
	}
	w.autoResumePass(now.Add(time.Hour))
	var count int
	_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes`).Scan(&count)
	if count != 0 {
		t.Fatal(count)
	}
	if err = s.ClearDashboardOverrides(); err != nil {
		t.Fatal(err)
	}
	w.autoResumePass(now.Add(time.Hour))
	var next int64
	_ = s.db.QueryRow(`SELECT next_check_at FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&next)
	if next != now.Add(time.Hour+5*time.Second).UnixMilli() {
		t.Fatal("missing owner not throttled", next)
	}
	_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes`).Scan(&count)
	if count != 0 {
		t.Fatal("unconfirmed owner consumed budget")
	}
	if err = s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "invalid"}); err != nil {
		t.Fatal(err)
	}
	enabled, err = autoResumeEnabled(s.db)
	if enabled || err == nil {
		t.Fatal("invalid value not fail closed")
	}
}

func TestAutoResumeManualRaceAndRejection(t *testing.T) {
	for _, manualRace := range []bool{false, true} {
		t.Run(map[bool]string{false: "rejection", true: "manual-race"}[manualRace], func(t *testing.T) {
			s, nc := workflowFixture(t)
			c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
			if err != nil {
				t.Fatal(err)
			}
			w := &Workflow{s: s, ctx: t.Context(), client: c}
			card := workflowCard(t, s)
			if err = w.MoveCard(card.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, _ := w.CardResult(card.ID)
			out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}
			if err = w.result(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			a, _ = w.CardResult(card.ID)
			target := client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
			sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
				raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 20})
				_ = m.Respond(raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			_ = nc.Flush()
			now := time.Now()
			if err = w.discoverAutoResumes(now); err != nil {
				t.Fatal(err)
			}
			due := now.Add(5 * time.Second)
			if manualRace {
				gate := make(chan struct{})
				results := make(chan error, 2)
				var wg sync.WaitGroup
				wg.Go(func() { <-gate; results <- w.resumeFailed(card.ID, false, resumeToken(a)) })
				wg.Go(func() { <-gate; results <- w.reserveFailedResume(card.ID, false, resumeToken(a), true, due) })
				close(gate)
				wg.Wait()
				close(results)
				wins := 0
				for e := range results {
					if e == nil {
						wins++
					}
				}
				if wins != 1 {
					t.Fatal("race winners", wins)
				}
			} else {
				w.autoResumePass(due)
			}
			var raw string
			var automatic bool
			if err = s.db.QueryRow(`SELECT request_json,automatic FROM failed_resumes WHERE status='pending'`).Scan(&raw, &automatic); err != nil {
				t.Fatal(err)
			}
			var req reportprotocol.FailedResume
			_ = json.Unmarshal([]byte(raw), &req)
			// Disabled settings cannot abandon this committed command; receipt still wins.
			if err = s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
				t.Fatal(err)
			}
			reply := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: target, Status: "rejected", Error: "host changed"}
			if err = w.finishFailedResume(req.ID, reply); err != nil {
				t.Fatal(err)
			}
			if automatic {
				if err = s.ClearDashboardOverrides(); err != nil {
					t.Fatal(err)
				}
				w.autoResumePass(due.Add(time.Hour))
				var state string
				_ = s.db.QueryRow(`SELECT state FROM auto_resumes WHERE task_id=?`, a.TaskID).Scan(&state)
				if state != "stopped" {
					t.Fatal("rejection not stopped", state)
				}
				var n int
				_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes`).Scan(&n)
				if n != 1 {
					t.Fatal("rejection issued new id")
				}
			}
			// Manual continuation remains possible, with no automatic budget charge.
			if err = w.ResumeFailed(card.ID); err != nil {
				t.Fatal(err)
			}
			var count int
			_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE automatic=1`).Scan(&count)
			expected := 0
			if automatic {
				expected = 1
			}
			if count != expected {
				t.Fatal("manual consumed budget", count)
			}
		})
	}
}

func TestAutoResumeStatusRendering(t *testing.T) {
	out := client.Outcome{Version: 1, TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}
	raw, _ := json.Marshal(out)
	a := Attempt{TaskID: "task", CardID: "card", State: "failed", RunState: "failed", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", OutcomeJSON: string(raw), Error: "authoritative runtime failure"}
	card := Card{ID: "card", Status: "Building"}
	status := autoResumeStatus{token: resumeToken(a), state: "scheduled", due: time.Unix(1700000000, 0).UnixMilli(), count: 2}
	d := pageData{cards: []Card{card}, attempts: []Attempt{a}, autoResumes: map[string]autoResumeStatus{"task": status}}
	for _, state := range []string{"scheduled", "stopped", "exhausted"} {
		status.state = state
		d.autoResumes["task"] = status
		var rendered bytes.Buffer
		if err := cardNote(d, card).Render(t.Context(), &rendered); err != nil {
			t.Fatal(err)
		}
		note := d.automaticResumeNote(card)
		if note == "" || !strings.Contains(rendered.String(), note) || !strings.Contains(rendered.String(), a.Error) {
			t.Fatal(rendered.String())
		}
	}
	card.Status = "Planning"
	d.v = viewParams{View: viewPlanning, Card: card.ID}
	d.cards = []Card{card}
	d.planning = map[string]PlanningDetail{card.ID: {Session: PlanningSession{State: "failed", Error: a.Error}, Turns: []PlanningTurn{{ID: "task"}}, CanResume: true, ResumeToken: resumeToken(a)}}
	var rendered bytes.Buffer
	if err := planningContent(d).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered.String(), d.automaticResumeNote(card)) || !strings.Contains(rendered.String(), a.Error) {
		t.Fatal(rendered.String())
	}
	status.token = "stale"
	d.autoResumes["task"] = status
	if d.automaticResumeNote(card) != "" {
		t.Fatal("obsolete status rendered")
	}
}
