package orchestrator

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mark3labs/bonnie/runtime"
)

var (
	errStopUnavailable     = errors.New("agent unavailable")
	errStopBusy            = errors.New("run busy")
	errStopSnapshot        = errors.New("cannot read agent state")
	errStopIdentityChanged = errors.New("run identity changed")
	errStopTurnChanged     = errors.New("active turn changed")
	errStopTargetChanged   = errors.New("cancellation target changed")
	errStopAuditFailed     = errors.New("cannot record cancellation")
	errStopOutcomeUnknown  = errors.New("cancellation outcome unknown")
)

type stopOutcome struct{ Attempt Attempt }

// StopRunTurn cancels only the observed exact turn. A request is not confirmation
// that execution stopped; active workflow cancellation retains its audit trail.
func (w *Workflow) StopRunTurn(ctx context.Context, attempt Attempt, turnID string) (stopOutcome, error) {
	fail := func(err error) (stopOutcome, error) { return stopOutcome{}, err }
	if !w.apiAvailable(ctx, attempt) {
		return fail(errStopUnavailable)
	}
	release, ok := w.runAPI.reserveSend(attempt)
	if !ok {
		return fail(errStopBusy)
	}
	defer release()
	snap, err := w.runAPI.snapshot(ctx, attempt)
	if err != nil {
		return fail(errStopSnapshot)
	}
	fresh, err := w.attemptByID(attempt.ID)
	if err != nil || fresh.AgentID != attempt.AgentID || fresh.RunID != attempt.RunID || fresh.RemoteAttemptID != attempt.RemoteAttemptID {
		return fail(errStopIdentityChanged)
	}
	policy := w.runPolicy(fresh, snap, true)
	if !policy.Stop || turnID == "" || turnID != snap.TurnID {
		return fail(errStopTurnChanged)
	}
	activeWorkflow := !fresh.Ready && policy.Mode == "steer"
	tx, err := w.s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail(errStopTargetChanged)
	}
	class, ce := classifyChat(tx, buildRun(fresh))
	chat, he := chatContinuation(tx, fresh.TaskID)
	_ = tx.Rollback()
	// Never issue a cancellation when its workflow classification is unknown.
	if ce != nil || he != nil {
		return fail(errStopTargetChanged)
	}
	if class == "followup" || chat {
		activeWorkflow = false
	}
	if activeWorkflow {
		if err = w.checkCancelLatest(fresh); err != nil {
			return fail(errStopTargetChanged)
		}
		if err = w.cancelAudit(fresh, turnID, "pending", ""); err != nil {
			return fail(errStopAuditFailed)
		}
	}
	result, err := w.runAPI.cancelTurn(ctx, fresh, turnID)
	if err == nil && (result.RunID != fresh.RunID || result.TurnID != turnID || result.Status != runtime.CancelRequested) {
		err = errStopOutcomeUnknown
	}
	if activeWorkflow {
		status := "requested"
		if err != nil {
			status = "uncertain"
		}
		auditErr := w.cancelAudit(fresh, turnID, status, "")
		if err == nil {
			err = auditErr
		}
	}
	if err != nil {
		return fail(errStopOutcomeUnknown)
	}
	return stopOutcome{Attempt: fresh}, nil
}
