package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// FailureTriage is a decision model's view of one failure generation.
type FailureTriage struct {
	// Permanent is the probability that resuming fails again the same way.
	Permanent float64
}

// FailureClassifier judges a sanitized run error. It is advisory: a permanent
// verdict only pauses automatic resume, and never authorizes an action.
type FailureClassifier func(ctx context.Context, errorText string) (FailureTriage, error)

const (
	// Hold automatic resume only on a clear verdict; a wrong hold costs an
	// operator click, a wrong resume costs one bounded retry.
	permanentFailureThreshold = 0.8
	triageTimeout             = 20 * time.Second
	triageBatch               = 8
	triageErrorLimit          = 4 << 10
)

// SetFailureClassifier enables failure triage before automatic resume.
func (w *Workflow) SetFailureClassifier(classify FailureClassifier) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.classifyFailure = classify
}

func (w *Workflow) failureClassifier() FailureClassifier {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.classifyFailure
}

// DecisionFailureClassifier asks a Kit decision model whether a failure is
// transient or permanent. decide is kit.Decide unless a test replaces it.
func DecisionFailureClassifier(model string, decide func(context.Context, kit.DecisionRequest) (*kit.DecisionResult, error)) FailureClassifier {
	if decide == nil {
		decide = kit.Decide
	}
	return func(ctx context.Context, errorText string) (FailureTriage, error) {
		res, err := decide(ctx, kit.DecisionRequest{
			Model: model,
			State: kit.DecisionState{Text: boundedFailure(errorText)},
			Questions: []kit.DecisionQuestion{kit.ChoiceQuestion("failure",
				"This error ended a coding agent's run. If the same run is resumed unchanged, will it fail again the same way?",
				kit.DecisionOption{Name: "transient", Description: "Likely to pass on retry: provider outage, rate limit, network error, timeout, interrupted process."},
				kit.DecisionOption{Name: "permanent", Description: "Will fail again until someone acts: invalid credentials or permissions, wrong configuration or model, context too long, request refused, quota exhausted, missing sandbox or workspace."},
			)},
		})
		if err != nil {
			return FailureTriage{}, err
		}
		a, err := res.Choice("failure")
		if err != nil {
			return FailureTriage{}, err
		}
		if a.Refused {
			return FailureTriage{}, errors.New("decision refused")
		}
		p, ok := a.Prob("permanent")
		if !ok {
			// Without a distribution, the choice alone is the verdict.
			p = 0
			if a.Choice == "permanent" {
				p = 1
			}
		}
		return FailureTriage{Permanent: p}, nil
	}
}

// CheckDecisionModel refuses a model that Kit cannot call as a decision
// model. Kit resolves the model, then rejects the empty request before it
// sends anything, so credentials are only checked by the first call.
func CheckDecisionModel(model string) error {
	if _, _, err := kit.ParseModelString(model); err != nil {
		return errors.New("invalid decision model selection; expected provider/model")
	}
	_, err := kit.Decide(context.Background(), kit.DecisionRequest{Model: model})
	if errors.Is(err, kit.ErrNotDecisionModel) || errors.Is(err, kit.ErrUnknownDecisionWire) {
		return fmt.Errorf("decision model %q: %w", model, err)
	}
	return nil
}

func boundedFailure(text string) string {
	if text == "" {
		return "(no error text)"
	}
	if len(text) <= triageErrorLimit {
		return text
	}
	text = text[:triageErrorLimit]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// triagePass classifies scheduled failure generations before they are due.
// It runs beside dispatch so provider I/O never delays the dispatch loop, and
// no transaction spans the model call. Each generation is classified once:
// the result, or "unavailable" after an error, is stored against its token.
func (w *Workflow) triagePass() {
	classify := w.failureClassifier()
	if classify == nil || w.ctx.Err() != nil {
		return
	}
	rows, err := w.s.db.Query(`SELECT task_id,planning,failure_token FROM auto_resumes WHERE state='scheduled' AND triage='' ORDER BY due_at,task_id LIMIT ?`, triageBatch)
	if err != nil {
		return
	}
	type pending struct {
		task, token string
		planning    bool
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if rows.Scan(&p.task, &p.planning, &p.token) != nil {
			break
		}
		batch = append(batch, p)
	}
	_ = rows.Close()
	for _, p := range batch {
		if w.ctx.Err() != nil {
			return
		}
		text, ok := w.failureText(p.task, p.planning, p.token)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(w.ctx, triageTimeout)
		verdict, err := classify(ctx, text)
		cancel()
		if w.ctx.Err() != nil {
			return
		}
		triage, state := "transient", "scheduled"
		switch {
		case err != nil:
			triage = "unavailable"
		case verdict.Permanent >= permanentFailureThreshold:
			triage, state = "permanent", "stopped"
		}
		res, err := w.s.db.Exec(`UPDATE auto_resumes SET triage=?,triage_probability=?,state=? WHERE task_id=? AND failure_token=? AND state='scheduled' AND triage=''`, triage, verdict.Permanent, state, p.task, p.token)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			w.log().Info("Failure triaged", "task_id", p.task, "triage", triage, "p_permanent", verdict.Permanent)
		}
	}
}

// failureText loads the stored error of the exact failure generation.
func (w *Workflow) failureText(task string, planning bool, token string) (string, bool) {
	a := Attempt{TaskID: task}
	query := `SELECT error,event_seq,outcome_json FROM workflow_attempts WHERE task_id=?`
	if planning {
		query = `SELECT error,event_seq,outcome_json FROM planning_turns WHERE id=?`
	}
	if err := w.s.db.QueryRow(query, task).Scan(&a.Error, &a.EventSeq, &a.OutcomeJSON); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			w.log().Debug("Failure triage read failed", "task_id", task)
		}
		return "", false
	}
	if resumeToken(a) != token {
		return "", false
	}
	return a.Error, true
}
