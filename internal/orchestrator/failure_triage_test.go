package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
)

// triageFixture has one failed build attempt whose owner reports it idle failed.
func triageFixture(t *testing.T, classify FailureClassifier) (*Workflow, Attempt, time.Time) {
	t.Helper()
	s, nc := workflowFixture(t)
	c, err := client.New(nc, client.Config{RootSubject: "bonnie", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &Workflow{s: s, ctx: t.Context(), client: c, classifyFailure: classify}
	card := workflowCard(t, s)
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := w.CardResult(card.ID)
	out := client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "provider: 401 invalid API key"}
	if err = w.result(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	target := client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}
	sub, err := nc.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
		raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 10})
		_ = m.Respond(raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	_ = nc.Flush()
	now := time.Unix(1700000000, 0)
	if err = w.discoverAutoResumes(now); err != nil {
		t.Fatal(err)
	}
	a, _ = w.CardResult(card.ID)
	return w, a, now.Add(autoResumeDelay(0))
}

func autoReservations(t *testing.T, w *Workflow) int {
	t.Helper()
	var n int
	if err := w.s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE automatic=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func triageRow(t *testing.T, w *Workflow, task string) (state, triage string, p float64) {
	t.Helper()
	if err := w.s.db.QueryRow(`SELECT state,triage,triage_probability FROM auto_resumes WHERE task_id=?`, task).Scan(&state, &triage, &p); err != nil {
		t.Fatal(err)
	}
	return state, triage, p
}

type countingClassifier struct {
	mu      sync.Mutex
	calls   int
	texts   []string
	verdict FailureTriage
	err     error
}

func (c *countingClassifier) classify(_ context.Context, text string) (FailureTriage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.texts = append(c.texts, text)
	return c.verdict, c.err
}

func TestFailureTriageHoldsPermanentFailure(t *testing.T) {
	t.Parallel()
	classifier := &countingClassifier{verdict: FailureTriage{Permanent: 0.95}}
	w, a, due := triageFixture(t, classifier.classify)
	// An untriaged generation is not resumed, even when due.
	w.autoResumePass(due)
	if n := autoReservations(t, w); n != 0 {
		t.Fatal("resumed before triage", n)
	}
	w.triagePass()
	w.triagePass()
	if classifier.calls != 1 || classifier.texts[0] != "provider: 401 invalid API key" {
		t.Fatalf("classifier calls %d texts %q", classifier.calls, classifier.texts)
	}
	if state, triage, p := triageRow(t, w, a.TaskID); state != "stopped" || triage != "permanent" || p != 0.95 {
		t.Fatal(state, triage, p)
	}
	w.autoResumePass(due.Add(time.Hour))
	if n := autoReservations(t, w); n != 0 {
		t.Fatal("permanent failure resumed", n)
	}
	// Rediscovery of the same generation keeps the hold.
	if err := w.discoverAutoResumes(due); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := triageRow(t, w, a.TaskID); state != "stopped" {
		t.Fatal("hold lost on rediscovery", state)
	}
	// A new failure generation is scheduled and triaged again.
	if _, err := w.s.db.Exec(`UPDATE workflow_attempts SET event_seq=event_seq+1 WHERE task_id=?`, a.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := w.discoverAutoResumes(due); err != nil {
		t.Fatal(err)
	}
	if state, triage, p := triageRow(t, w, a.TaskID); state != "scheduled" || triage != "" || p != 0 {
		t.Fatal("new generation kept old triage", state, triage, p)
	}
	w.triagePass()
	if classifier.calls != 2 {
		t.Fatal("new generation not triaged", classifier.calls)
	}
}

func TestFailureTriageResumesTransientAndUnavailable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, triage string
		classifier   *countingClassifier
	}{
		{"transient", "transient", &countingClassifier{verdict: FailureTriage{Permanent: 0.3}}},
		{"unavailable", "unavailable", &countingClassifier{err: errors.New("provider down")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, a, due := triageFixture(t, tc.classifier.classify)
			w.triagePass()
			if state, triage, _ := triageRow(t, w, a.TaskID); state != "scheduled" || triage != tc.triage {
				t.Fatal(state, triage)
			}
			w.autoResumePass(due)
			if n := autoReservations(t, w); n != 1 {
				t.Fatal("triaged failure not resumed", n)
			}
			w.triagePass()
			if tc.classifier.calls != 1 {
				t.Fatal("classified twice", tc.classifier.calls)
			}
		})
	}
}

func TestAutoResumeWithoutClassifierSkipsTriage(t *testing.T) {
	t.Parallel()
	w, a, due := triageFixture(t, nil)
	w.triagePass()
	w.autoResumePass(due)
	if n := autoReservations(t, w); n != 1 {
		t.Fatal("resume waited for disabled triage", n)
	}
	if _, triage, _ := triageRow(t, w, a.TaskID); triage != "" {
		t.Fatal("triaged without classifier", triage)
	}
}

func TestDecisionFailureClassifier(t *testing.T) {
	t.Parallel()
	answer := func(a kit.DecisionAnswer, err error, seen *kit.DecisionRequest) func(context.Context, kit.DecisionRequest) (*kit.DecisionResult, error) {
		return func(_ context.Context, req kit.DecisionRequest) (*kit.DecisionResult, error) {
			*seen = req
			if err != nil {
				return nil, err
			}
			a.ID, a.Type = "failure", kit.DecisionChoice
			return &kit.DecisionResult{Answers: []kit.DecisionAnswer{a}}, nil
		}
	}
	dist := []kit.DecisionProbability{{Index: 0, Name: "transient", Probability: 0.15}, {Index: 1, Name: "permanent", Probability: 0.85}}
	for _, tc := range []struct {
		name    string
		answer  kit.DecisionAnswer
		err     error
		want    float64
		wantErr bool
	}{
		{"distribution", kit.DecisionAnswer{Choice: "permanent", Distribution: dist}, nil, 0.85, false},
		{"choice only permanent", kit.DecisionAnswer{Choice: "permanent"}, nil, 1, false},
		{"choice only transient", kit.DecisionAnswer{Choice: "transient"}, nil, 0, false},
		{"refused", kit.DecisionAnswer{Refused: true}, nil, 0, true},
		{"provider error", kit.DecisionAnswer{}, errors.New("down"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen kit.DecisionRequest
			got, err := DecisionFailureClassifier("ollama/decider", answer(tc.answer, tc.err, &seen))(t.Context(), "boom")
			if (err != nil) != tc.wantErr || got.Permanent != tc.want {
				t.Fatalf("got %v, %v", got, err)
			}
			if seen.Model != "ollama/decider" || seen.State.Text != "boom" || len(seen.Questions) != 1 || seen.Questions[0].ID != "failure" {
				t.Fatalf("request %+v", seen)
			}
		})
	}
}

func TestBoundedFailure(t *testing.T) {
	t.Parallel()
	if boundedFailure("") != "(no error text)" {
		t.Fatal("empty")
	}
	got := boundedFailure(strings.Repeat("é", triageErrorLimit))
	if len(got) > triageErrorLimit || !utf8.ValidString(got) {
		t.Fatal(len(got))
	}
}

func TestCheckDecisionModel(t *testing.T) {
	t.Parallel()
	if err := CheckDecisionModel("secret-selection"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe or missing error: %v", err)
	}
	if err := CheckDecisionModel("openrouter/openai/gpt-4o-mini"); !errors.Is(err, kit.ErrNotDecisionModel) {
		t.Fatalf("chat model accepted: %v", err)
	}
	if err := CheckDecisionModel("ollama/pplx-decider"); err != nil {
		t.Fatalf("decision model refused: %v", err)
	}
}

func TestPermanentTriageStatusNote(t *testing.T) {
	t.Parallel()
	out := client.Outcome{Version: 1, TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed}
	raw, _ := json.Marshal(out)
	a := Attempt{TaskID: "task", CardID: "card", State: "failed", RunState: "failed", AgentID: "agent", RunID: "run", RemoteAttemptID: "remote", OutcomeJSON: string(raw), Error: "invalid API key"}
	card := Card{ID: "card", Status: "Building"}
	d := pageData{cards: []Card{card}, attempts: []Attempt{a}, autoResumes: map[string]autoResumeStatus{"task": {token: resumeToken(a), state: "stopped", triage: "permanent", permanent: 0.92}}}
	var rendered bytes.Buffer
	if err := cardNote(d, card).Render(t.Context(), &rendered); err != nil {
		t.Fatal(err)
	}
	note := d.automaticResumeNote(card)
	if !strings.Contains(note, "permanent (92%)") || !strings.Contains(rendered.String(), "permanent (92%)") {
		t.Fatal(note, rendered.String())
	}
}

// TestLiveFailureTriage asks a real decision model. JAWA_DECISION_MODEL
// overrides the default OpenRouter model.
func TestLiveFailureTriage(t *testing.T) {
	if os.Getenv("JAWA_DECISION_LIVE") != "1" {
		t.Skip("set JAWA_DECISION_LIVE=1 and the provider credential")
	}
	model := orchestratorEnv("JAWA_DECISION_MODEL", "openrouter/typesafe/jev-1.14")
	if err := CheckDecisionModel(model); err != nil {
		t.Fatal(err)
	}
	classify := DecisionFailureClassifier(model, nil)
	for _, tc := range []struct {
		text      string
		permanent bool
	}{
		{"provider: 401 Unauthorized: invalid API key", true},
		{"provider: 503 Service Unavailable: upstream overloaded, please retry", false},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		got, err := classify(ctx, tc.text)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: P(permanent) = %.3f for %q", model, got.Permanent, tc.text)
		if (got.Permanent >= permanentFailureThreshold) != tc.permanent {
			t.Fatalf("%q: P(permanent) = %v", tc.text, got.Permanent)
		}
	}
}
