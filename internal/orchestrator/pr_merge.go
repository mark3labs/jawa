package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

var errMergeLocked = errors.New("merge pending or uncertain; refresh ready PRs to reconcile before changing this card")
var errMergeRejected = errors.New("merge not confirmed: refresh and confirm again; check repository policy, selected method and token merge permissions")

func noUnresolvedMerge(tx *sql.Tx, id string) error {
	var locked bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pr_merges WHERE card_id=? AND state IN ('pending','uncertain'))`, id).Scan(&locked); err != nil {
		return err
	}
	if locked {
		return errMergeLocked
	}
	return nil
}

func mergeEligible(c Card, all []Attempt, check PRCheck, pending bool) bool {
	latest := latestAttempt(c.ID, all)
	for _, a := range all {
		if a.CardID == c.ID && a.Error == resumePendingNotice {
			pending = true
		}
	}
	return !c.Archived && c.Status == "Done" && !pending && canAction(c, all, "fix") && len(latest) == 1 && check.AttemptID == latest[0].ID && check.PRURL == latest[0].PRURL && check.State == "clear" && !check.Manual && providerSHA.MatchString(check.HeadSHA) && providerSHA.MatchString(check.BaseSHA)
}

func mergeSnapshot(c Card, a Attempt, p Project, head, base string) string {
	sum := sha256.Sum256([]byte(forceReadyToken(c, a, p) + ":" + head + ":" + base))
	return hex.EncodeToString(sum[:])
}

func (w *Workflow) SetProviderMerger(merger ProviderMerger) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.merger = merger
}

// MergePR reserves authority before network I/O. Client fields are expectations,
// never remote coordinates. No transaction spans a provider request.
func (w *Workflow) MergePR(ctx context.Context, cardID, operation, snapshot, method, operator string) error {
	if err := ctx.Err(); err != nil {
		return errMergeRejected
	}
	if len(operation) != 32 || dialogErrorID("merge", operation) != "merge-error-"+operation || snapshot == "" || !validMergeMethod(method) || operator == "" {
		return errMergeRejected
	}
	w.mu.RLock()
	merger := w.merger
	w.mu.RUnlock()
	if merger == nil {
		return errors.New("provider merging unavailable")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var oldCard, oldSnapshot, oldMethod, state string
	err = tx.QueryRow(`SELECT card_id,snapshot,method,state FROM pr_merges WHERE id=?`, operation).Scan(&oldCard, &oldSnapshot, &oldMethod, &state)
	if err == nil {
		if oldCard != cardID || oldSnapshot != snapshot || oldMethod != method {
			return errMergeRejected
		}
		return mergeOutcome(state)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	c, p, all, pending, err := readySnapshot(tx, cardID)
	if err != nil {
		return err
	}
	if err = tx.QueryRow(`SELECT archived FROM cards WHERE id=?`, cardID).Scan(&c.Archived); err != nil {
		return err
	}
	if err = noActiveAttempts(tx, cardID); err != nil {
		return err
	}
	var check PRCheck
	err = tx.QueryRow(`SELECT attempt_id,pr_url,state,head_sha,base_sha FROM workflow_pr_checks WHERE attempt_id=(SELECT id FROM workflow_attempts WHERE card_id=? ORDER BY number DESC LIMIT 1) AND provider=? AND repo=? AND base_branch=? AND NOT EXISTS(SELECT 1 FROM workflow_pr_merge_overrides m WHERE m.attempt_id=workflow_pr_checks.attempt_id AND m.pr_url=workflow_pr_checks.pr_url AND m.provider=? AND m.repo=? AND m.base_branch=?)`, cardID, p.Provider, p.Repo, p.BaseBranch, p.Provider, p.Repo, p.BaseBranch).Scan(&check.AttemptID, &check.PRURL, &check.State, &check.HeadSHA, &check.BaseSHA)
	if err != nil || !mergeEligible(c, all, check, pending) {
		return errMergeRejected
	}
	a := latestAttempt(cardID, all)[0]
	if mergeSnapshot(c, a, p, check.HeadSHA, check.BaseSHA) != snapshot {
		return errMergeRejected
	}
	now := time.Now().UTC().UnixMilli()
	_, err = tx.Exec(`INSERT INTO pr_merges(id,card_id,attempt_id,pr_url,snapshot,provider,repo,base_branch,head_sha,method,operator,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?,?)`, operation, cardID, a.ID, a.PRURL, snapshot, p.Provider, p.Repo, p.BaseBranch, check.HeadSHA, method, operator, now, now)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, time.UnixMilli(now).Add(20*time.Second))
	defer cancel()
	if w.ctx != nil {
		stop := context.AfterFunc(w.ctx, cancel)
		defer stop()
	}
	state = merger(ctx, a, check.HeadSHA, method)
	if state != "merged" && state != "rejected" {
		state = "uncertain"
	}
	// Finalization must survive a disconnected browser, but remains bounded.
	finish, cancelFinish := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelFinish()
	if err = w.finishMerge(finish, operation, state); err != nil {
		return errMergeLocked
	}
	return mergeOutcome(state)
}

func mergeOutcome(state string) error {
	if state == "merged" {
		return nil
	}
	if state == "pending" || state == "uncertain" {
		return errMergeLocked
	}
	return errMergeRejected
}

func (w *Workflow) finishMerge(ctx context.Context, operation, state string) error {
	tx, err := w.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE projects SET id=id WHERE 0`); err != nil {
		return err
	}
	// Audit is independent of attachment: a replaced attempt/configuration must
	// never acquire old merge evidence. Unresolved operations fence local changes.
	if _, err = tx.ExecContext(ctx, `UPDATE pr_merges SET state=?,updated_at=? WHERE id=? AND state IN ('pending','uncertain')`, state, time.Now().UTC().UnixMilli(), operation); err != nil {
		return err
	}
	if state == "merged" {
		_, err = tx.ExecContext(ctx, `INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,base_sha,error,checked_at)
 SELECT m.attempt_id,m.pr_url,m.provider,m.repo,m.base_branch,'merged',m.head_sha,'','',?
 FROM pr_merges m JOIN workflow_attempts a ON a.id=m.attempt_id JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id
 WHERE m.id=? AND m.state='merged' AND a.pr_url=m.pr_url AND a.ready=1 AND a.state='ready' AND c.status='Done'
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND p.provider=m.provider AND p.repo=m.repo AND p.base_branch=m.base_branch
 ON CONFLICT(attempt_id) DO UPDATE SET pr_url=excluded.pr_url,provider=excluded.provider,repo=excluded.repo,base_branch=excluded.base_branch,state='merged',head_sha=excluded.head_sha,base_sha='',error='',checked_at=excluded.checked_at`, time.Now().UTC().UnixMilli(), operation)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ReconcileMerges never writes to a provider. Even an open PR releases a crashed
// reservation only after its entire bounded write window has elapsed.
func (w *Workflow) ReconcileMerges(ctx context.Context, checker ConflictChecker) error {
	rows, err := w.s.db.QueryContext(ctx, `SELECT id,attempt_id,head_sha,created_at FROM pr_merges WHERE state IN ('pending','uncertain') ORDER BY created_at LIMIT 16`)
	if err != nil {
		return err
	}
	type item struct {
		id, attempt, head string
		created           int64
	}
	var items []item
	for rows.Next() {
		var i item
		if err = rows.Scan(&i.id, &i.attempt, &i.head, &i.created); err != nil {
			break
		}
		items = append(items, i)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, i := range items {
		if time.Since(time.UnixMilli(i.created)) < 25*time.Second {
			continue
		}
		a, e := scanAttempt(w.s.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, i.attempt))
		if e != nil {
			continue
		}
		check, e := checker(ctx, a)
		if e != nil {
			continue
		}
		state := "uncertain"
		if check.State == "merged" && check.HeadSHA == i.head {
			state = "merged"
		} else if check.State == "clear" || check.State == "conflict" || check.State == "closed" || (check.State == "unknown" && check.Open && providerSHA.MatchString(check.HeadSHA)) {
			state = "rejected"
		}
		if e = w.finishMerge(ctx, i.id, state); e != nil {
			return e
		}
	}
	return nil
}
