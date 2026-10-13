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

// PollPRConflicts checks latest ready PRs on non-archived Done cards. It never moves a card,
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
 WHERE c.status='Done' AND c.archived=0 AND a.ready=1 AND a.state='ready' AND a.pr_url<>''
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
	_, err = w.checkPRBatch(ctx, monitor, ids)
	return err
}

// checkPRBatch runs under monitor.mu; no database transaction spans provider I/O.
func (w *Workflow) checkPRBatch(ctx context.Context, monitor *conflictMonitor, ids []string) (completed int, err error) {
	for _, id := range ids {
		if err = ctx.Err(); err != nil {
			return completed, err
		}
		// Revalidate selected IDs before provider I/O; visibility may have changed.
		a, e := scanAttempt(w.s.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?
 AND ready=1 AND state='ready' AND pr_url<>''
 AND number=(SELECT max(number) FROM workflow_attempts latest WHERE latest.card_id=workflow_attempts.card_id)
 AND EXISTS(SELECT 1 FROM cards c WHERE c.id=workflow_attempts.card_id AND c.status='Done' AND c.archived=0)`, id))
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return completed, e
		}
		var provider, repo, base string
		if e = w.s.db.QueryRowContext(ctx, `SELECT p.provider,p.repo,p.base_branch FROM projects p JOIN cards c ON c.project_id=p.id WHERE c.id=?`, a.CardID).Scan(&provider, &repo, &base); e != nil {
			return completed, e
		}
		check, checkErr := monitor.checker(ctx, a)
		if ctx.Err() != nil {
			return completed, ctx.Err()
		}
		if checkErr != nil {
			// Do not persist transport errors/credentials or a previous conflict as current.
			check.State, check.HeadSHA, check.BaseSHA, check.Error = "error", "", "", "provider check failed"
		}
		switch check.State {
		case "conflict", "clear", "unknown", "error", "merged", "closed":
		default:
			check.State = "unknown"
		}
		// CAS against latest attempt, lane, archive state and project config after the network wait.
		_, e = w.s.db.ExecContext(ctx, `INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,base_sha,error,checked_at)
 SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(
 SELECT 1 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id
 WHERE a.id=? AND a.pr_url=? AND a.ready=1 AND a.state='ready' AND c.status='Done' AND c.archived=0
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id)
 AND p.provider=? AND p.repo=? AND p.base_branch=?)
 ON CONFLICT(attempt_id) DO UPDATE SET pr_url=excluded.pr_url,provider=excluded.provider,repo=excluded.repo,
 base_branch=excluded.base_branch,state=excluded.state,head_sha=excluded.head_sha,base_sha=excluded.base_sha,error=excluded.error,checked_at=excluded.checked_at`,
			a.ID, a.PRURL, provider, repo, base, check.State, check.HeadSHA, check.BaseSHA, check.Error, time.Now().UTC().UnixMilli(), a.ID, a.PRURL, provider, repo, base)
		if e != nil {
			return completed, e
		}
		completed++
	}
	return completed, nil
}

// PRRefreshResult describes a synchronous bounded batch and the work left for polling.
// Checking includes queued checks; CheckedAt is zero until fresh evidence arrives.
type PRRefreshResult struct {
	Targeted int `json:"targeted"`
	Checked  int `json:"checked"`
	Queued   int `json:"queued"`
}

var ErrPRRefreshBusy = errors.New("PR checks already running")
var ErrPRChecksUnavailable = errors.New("PR checks unavailable")

// RefreshPRChecks bypasses the minute cooldown for a project's eligible non-archived Done cards.
// Empty cardID means the whole project. Invalid projects, unrelated or archived
// cards, and cards without a latest verified ready PR return sql.ErrNoRows without writes.
// All targets are durably marked checking, then at most 16 are checked in 30s.
// Cancellation leaves unfinished targets queued for normal polling (and restart).
func (w *Workflow) RefreshPRChecks(ctx context.Context, projectID, cardID string) (result PRRefreshResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if projectID == "" {
		return result, sql.ErrNoRows
	}
	var exists int
	if err = w.s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=?`, projectID).Scan(&exists); err != nil {
		return result, err
	}
	w.mu.RLock()
	monitor := w.conflicts
	w.mu.RUnlock()
	if monitor == nil || monitor.checker == nil {
		return result, ErrPRChecksUnavailable
	}
	if !monitor.mu.TryLock() {
		return result, ErrPRRefreshBusy
	}
	defer monitor.mu.Unlock()
	// One statement validates scope and invalidates all old evidence atomically.
	res, err := w.s.db.ExecContext(ctx, `INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,base_sha,error,checked_at)
 SELECT a.id,a.pr_url,p.provider,p.repo,p.base_branch,'checking','','','',0
 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id
 WHERE c.project_id=? AND (?='' OR c.id=?) AND c.status='Done' AND c.archived=0
 AND a.ready=1 AND a.state='ready' AND a.pr_url<>''
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 ON CONFLICT(attempt_id) DO UPDATE SET pr_url=excluded.pr_url,provider=excluded.provider,repo=excluded.repo,
 base_branch=excluded.base_branch,state='checking',head_sha='',base_sha='',error='',checked_at=0`, projectID, cardID, cardID)
	if err != nil {
		return result, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return result, err
	}
	result.Targeted, result.Queued = int(count), int(count)
	if cardID != "" && count == 0 {
		return result, sql.ErrNoRows
	}
	rows, err := w.s.db.QueryContext(ctx, `SELECT a.id FROM workflow_attempts a JOIN cards c ON c.id=a.card_id
 JOIN workflow_pr_checks p ON p.attempt_id=a.id
 WHERE c.project_id=? AND (?='' OR c.id=?) AND p.state='checking' AND p.checked_at=0
 AND c.status='Done' AND c.archived=0 AND a.ready=1 AND a.state='ready' AND a.pr_url=p.pr_url
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 ORDER BY a.id LIMIT 16`, projectID, cardID, cardID)
	if err != nil {
		return result, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return result, err
	}
	result.Checked, err = w.checkPRBatch(ctx, monitor, ids)
	result.Queued = result.Targeted - result.Checked
	return result, err
}

// CardPRCheck returns current evidence for a Done card's latest ready attempt.
// sql.ErrNoRows means no applicable check, not clear/no conflict. Historical
// evidence is hidden during repair and after project configuration changes.
func (w *Workflow) CardPRCheck(id string) (PRCheck, error) {
	var p PRCheck
	var marked int64
	err := w.s.db.QueryRow(`SELECT m.attempt_id,m.pr_url,m.marked_at
 FROM workflow_pr_merge_overrides m JOIN workflow_attempts a ON a.id=m.attempt_id
 JOIN cards c ON c.id=a.card_id JOIN projects project ON project.id=c.project_id
 WHERE c.id=? AND c.status='Done' AND a.ready=1 AND a.state='ready' AND a.pr_url=m.pr_url
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND m.provider=project.provider AND m.repo=project.repo AND m.base_branch=project.base_branch`, id).Scan(&p.AttemptID, &p.PRURL, &marked)
	if err == nil {
		p.State, p.Manual, p.MarkedAt = "merged", true, time.UnixMilli(marked).UTC()
		return p, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	var checked int64
	err = w.s.db.QueryRow(`SELECT p.attempt_id,p.pr_url,p.state,p.head_sha,p.base_sha,p.error,p.checked_at
 FROM workflow_pr_checks p JOIN workflow_attempts a ON a.id=p.attempt_id
 JOIN cards c ON c.id=a.card_id JOIN projects project ON project.id=c.project_id
 WHERE c.id=? AND c.status='Done' AND a.ready=1 AND a.state='ready' AND a.pr_url=p.pr_url
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND p.provider=project.provider AND p.repo=project.repo AND p.base_branch=project.base_branch`, id).Scan(&p.AttemptID, &p.PRURL, &p.State, &p.HeadSHA, &p.BaseSHA, &p.Error, &checked)
	if err == nil {
		if checked != 0 {
			p.CheckedAt = time.UnixMilli(checked).UTC()
		}
	}
	return p, err
}
