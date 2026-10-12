package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MarkPRMerged records local display metadata only, never provider evidence or
// readiness. The write lock serializes this validation with repairs/new attempts.
func (w *Workflow) MarkPRMerged(ctx context.Context, cardID, expectedAttemptID, expectedPRURL string) error {
	tx, err := w.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET id=id WHERE 0`); err != nil {
		return err
	}
	var status, provider, repo, base string
	if err = tx.QueryRowContext(ctx, `SELECT c.status,p.provider,p.repo,p.base_branch FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, cardID).Scan(&status, &provider, &repo, &base); err != nil {
		return workflowCardError(err)
	}
	latest, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1`, cardID))
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("marking merged requires a verified Ready pull request")
	}
	if err != nil {
		return err
	}
	if status != "Done" || !latest.Ready || latest.State != "ready" || latest.PRURL == "" || latest.ID != expectedAttemptID || latest.PRURL != expectedPRURL {
		return errors.New("marking merged requires the current verified Ready pull request; refresh the board and try again")
	}
	if err = noActiveAttempts(tx, cardID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_pr_merge_overrides(attempt_id,pr_url,provider,repo,base_branch,marked_at) VALUES(?,?,?,?,?,?)
 ON CONFLICT(attempt_id) DO UPDATE SET pr_url=excluded.pr_url,provider=excluded.provider,repo=excluded.repo,base_branch=excluded.base_branch,marked_at=excluded.marked_at
 WHERE pr_url<>excluded.pr_url OR provider<>excluded.provider OR repo<>excluded.repo OR base_branch<>excluded.base_branch`, latest.ID, latest.PRURL, provider, repo, base, time.Now().UTC().UnixMilli())
	if err != nil {
		return err
	}
	return tx.Commit()
}
