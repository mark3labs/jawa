package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// conflictMonitor serializes polls and caps each batch. Oldest checks are served
// first so a large board cannot starve cards beyond the batch limit.
type conflictMonitor struct {
	mu      sync.Mutex
	checker ConflictChecker
}

// SetProviderChecks installs readiness verification and conflict polling together.
func (w *Workflow) SetProviderChecks(v Verifier, checker ConflictChecker) {
	w.mu.Lock()
	w.verifier = v
	w.conflicts = &conflictMonitor{checker: checker}
	w.mu.Unlock()
}

// PollPRConflicts checks latest ready PRs on Done cards. It never moves a card,
// clears readiness, queues repairs, or holds a DB transaction over network I/O.
// A poll is limited to 16 cards and 30 seconds; each provider call is also bounded.
func (w *Workflow) PollPRConflicts(ctx context.Context) error {
	w.mu.RLock()
	monitor := w.conflicts
	w.mu.RUnlock()
	if monitor == nil || monitor.checker == nil {
		return nil
	}
	if !monitor.mu.TryLock() {
		return nil
	}
	defer monitor.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Read IDs first and close rows before using the Store's single connection.
	rows, err := w.s.db.QueryContext(ctx, `SELECT a.id FROM workflow_attempts a
 JOIN cards c ON c.id=a.card_id
 LEFT JOIN workflow_pr_checks p ON p.attempt_id=a.id
 WHERE c.status='Done' AND a.ready=1 AND a.state='ready' AND a.pr_url<>''
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)
 AND (p.checked_at IS NULL OR p.checked_at<?)
 ORDER BY coalesce(p.checked_at,0),a.id LIMIT 16`, time.Now().Add(-time.Minute).UnixMilli())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return err
		}
		a, e := scanAttempt(w.s.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, id))
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return e
		}
		var provider, repo, base string
		if e = w.s.db.QueryRowContext(ctx, `SELECT p.provider,p.repo,p.base_branch FROM projects p JOIN cards c ON c.project_id=p.id WHERE c.id=?`, a.CardID).Scan(&provider, &repo, &base); e != nil {
			return e
		}
		check, checkErr := monitor.checker(ctx, a)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkErr != nil {
			// Do not persist transport errors/credentials or a previous conflict as current.
			check.State, check.HeadSHA, check.Error = "error", "", "provider check failed"
		}
		switch check.State {
		case "conflict", "clear", "unknown", "error", "merged", "closed":
		default:
			check.State = "unknown"
		}
		// CAS against latest attempt, lane and project config after the network wait.
		_, e = w.s.db.ExecContext(ctx, `INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,error,checked_at)
 SELECT ?,?,?,?,?,?,?,?,? WHERE EXISTS(
 SELECT 1 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id
 WHERE a.id=? AND a.pr_url=? AND a.ready=1 AND a.state='ready' AND c.status='Done'
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)
 AND p.provider=? AND p.repo=? AND p.base_branch=?)
 ON CONFLICT(attempt_id) DO UPDATE SET pr_url=excluded.pr_url,provider=excluded.provider,repo=excluded.repo,
 base_branch=excluded.base_branch,state=excluded.state,head_sha=excluded.head_sha,error=excluded.error,checked_at=excluded.checked_at`,
			a.ID, a.PRURL, provider, repo, base, check.State, check.HeadSHA, check.Error, time.Now().UTC().UnixMilli(), a.ID, a.PRURL, provider, repo, base)
		if e != nil {
			return e
		}
	}
	return nil
}

// CardPRCheck returns current evidence for a Done card's latest ready attempt.
// sql.ErrNoRows means no applicable check, not clear/no conflict. Historical
// evidence is hidden during repair and after project configuration changes.
func (w *Workflow) CardPRCheck(id string) (PRCheck, error) {
	var p PRCheck
	var checked int64
	err := w.s.db.QueryRow(`SELECT p.attempt_id,p.pr_url,p.state,p.head_sha,p.error,p.checked_at
 FROM workflow_pr_checks p JOIN workflow_attempts a ON a.id=p.attempt_id
 JOIN cards c ON c.id=a.card_id JOIN projects project ON project.id=c.project_id
 WHERE c.id=? AND c.status='Done' AND a.ready=1 AND a.state='ready' AND a.pr_url=p.pr_url
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND p.provider=project.provider AND p.repo=project.repo AND p.base_branch=project.base_branch`, id).Scan(&p.AttemptID, &p.PRURL, &p.State, &p.HeadSHA, &p.Error, &checked)
	if err == nil {
		p.CheckedAt = time.UnixMilli(checked).UTC()
	}
	return p, err
}
