package orchestrator

import (
	"context"
	"slices"
	"strings"
	"time"

	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/runapitoken"
)

// RunRecord is a read-only history projection, never workflow authority.
// Only implementation records carry an Attempt; planners cannot enter build guards.
type RunRecord struct {
	Mode, ID, CardID, TaskID, AgentID, RunID, RemoteAttemptID                        string
	Number                                                                           int
	State, RunState, Result, Error, SessionState, InteractionStatus, InteractionKind string
	CreatedAt, UpdatedAt                                                             time.Time
	Superseded, PendingResume                                                        bool
	Build                                                                            Attempt
}

type runIdentity struct{ AgentID, RunID, RemoteAttemptID string }
type runObserver interface{ observationIdentity() runIdentity }

func (a Attempt) observationIdentity() runIdentity {
	return runIdentity{a.AgentID, a.RunID, a.RemoteAttemptID}
}
func (a RunRecord) observationIdentity() runIdentity {
	return runIdentity{a.AgentID, a.RunID, a.RemoteAttemptID}
}
func (a runIdentity) observationIdentity() runIdentity { return a }

func buildRun(a Attempt) RunRecord {
	mode := "Build"
	if a.RepairPRURL != "" {
		mode = "Fix"
	}
	return RunRecord{Mode: mode, ID: a.ID, CardID: a.CardID, TaskID: a.TaskID, Number: a.Number, AgentID: a.AgentID, RunID: a.RunID, RemoteAttemptID: a.RemoteAttemptID, State: a.State, RunState: a.RunState, Result: a.Result, Error: a.Error, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, Build: a}
}
func planningRun(t sqlc.ListPlanningRunHistoryRow) RunRecord {
	return RunRecord{Mode: "Planning", ID: t.ID, CardID: t.CardID, TaskID: t.ID, Number: int(t.Number), AgentID: t.AgentID, RunID: t.RunID, RemoteAttemptID: t.RemoteAttemptID, State: t.State, RunState: t.State, Result: t.Result, Error: t.Error, SessionState: t.SessionState, InteractionStatus: t.InteractionStatus, InteractionKind: t.InteractionKind, Superseded: t.Superseded != 0, PendingResume: t.PendingResume != 0, CreatedAt: time.UnixMilli(t.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(t.UpdatedAt).UTC()}
}
func (s *Store) runHistory(ctx context.Context, attempts []Attempt) ([]RunRecord, error) {
	turns, err := sqlc.New(s.db).ListPlanningRunHistory(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(turns)+len(attempts))
	for _, a := range attempts {
		out = append(out, buildRun(a))
	}
	for _, t := range turns {
		out = append(out, planningRun(t))
	}
	slices.SortFunc(out, func(a, b RunRecord) int {
		if n := b.CreatedAt.Compare(a.CreatedAt); n != 0 {
			return n
		}
		if n := strings.Compare(a.Mode, b.Mode); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}
func (w *Workflow) historyByID(ctx context.Context, mode, id string) (RunRecord, error) {
	if mode == "Planning" {
		t, err := sqlc.New(w.s.db).GetPlanningRunHistory(ctx, id)
		if err != nil {
			return RunRecord{}, err
		}
		return planningRun(sqlc.ListPlanningRunHistoryRow(t)), nil
	}
	a, err := w.attemptByID(id)
	if err != nil {
		return RunRecord{}, err
	}
	return buildRun(a), nil
}
func (a RunRecord) pinned() bool {
	_, err := runapitoken.Subject("bonnie", a.AgentID)
	return err == nil && validRunDestination(a.RunID) && validRunDestination(a.RemoteAttemptID)
}
func (a RunRecord) path(tab string) string { return a.basePath() + "?tab=" + tab }
func (a RunRecord) basePath() string {
	if a.Mode == "Planning" {
		return "/runs/planning/" + a.ID
	}
	return "/runs/" + a.ID
}
func (a RunRecord) label() string {
	if a.Mode != "Planning" {
		return runLabel(a.Build)
	}
	switch a.State {
	case "cancelled":
		return "Cancelled"
	case "completed":
		return "Completed"
	}
	if a.PendingResume || a.Error == resumePendingNotice {
		return "Resuming"
	}
	if a.State == "failed" {
		return "Failed"
	}
	if a.InteractionStatus == "queued" {
		return "Resuming"
	}
	if a.InteractionStatus == "pending" {
		if a.InteractionKind == "propose_spec" {
			return "Awaiting review"
		}
		return "Needs input"
	}
	if a.State == "queued" {
		return "Queued"
	}
	if a.State == "submitted" || a.State == "running" {
		return "Investigating"
	}
	// Answer dispatch resumes the same task even before turn bookkeeping changes.
	if a.State == "waiting" && a.InteractionStatus == "answered" {
		return "Resuming"
	}
	if !a.Superseded && a.SessionState == "approved" {
		return "Approved"
	}
	if a.State == "waiting" {
		return "Needs input"
	}
	return "Completed"
}
func (a RunRecord) tone() string {
	if a.Mode != "Planning" {
		return runTone(a.Build)
	}
	switch a.label() {
	case "Resuming":
		return "resuming"
	case "Investigating":
		return "running"
	case "Needs input", "Awaiting review":
		return "blocked"
	case "Failed":
		return "failed"
	}
	return "queued"
}
func (a RunRecord) matches(state string) bool {
	if a.Mode != "Planning" {
		return matchesRunState(a.Build, state)
	}
	switch state {
	case "active":
		return a.label() == "Queued" || a.label() == "Investigating" || a.label() == "Resuming"
	case "ready":
		return false
	case "blocked", "failed":
		return a.tone() == state
	}
	return true
}
func filterHistory(runs []RunRecord, v viewParams) []RunRecord {
	out := make([]RunRecord, 0, len(runs))
	for _, a := range runs {
		if (v.Card == "" || a.CardID == v.Card) && a.matches(v.State) {
			out = append(out, a)
		}
	}
	return out
}
func countHistory(runs []RunRecord, state string) int {
	n := 0
	for _, a := range runs {
		if a.matches(state) {
			n++
		}
	}
	return n
}
func (d pageData) historyLabel(a RunRecord) string {
	if a.Mode != "Planning" {
		return d.runLabel(a.Build)
	}
	return a.label()
}
func (d pageData) hasRuns(card string) bool {
	return len(filterHistory(d.runs, viewParams{Card: card, State: "all"})) > 0 || len(latestAttempt(card, d.attempts)) > 0
}
