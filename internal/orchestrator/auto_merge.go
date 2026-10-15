package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

func validAutoMergeMode(mode string) bool { return mode == "inherit" || mode == "on" || mode == "off" }
func autoMergeEffective(global bool, mode string) bool {
	return validAutoMergeMode(mode) && (mode == "on" || mode == "inherit" && global)
}
func submittedAutoMergeMode(raw string) (string, error) {
	if raw == "" {
		raw = "inherit"
	}
	if !validAutoMergeMode(raw) {
		return "", errors.New("invalid auto merge mode")
	}
	return raw, nil
}
func (w *Workflow) SetCardAutoMerge(id, expected, mode string) error {
	if !validAutoMergeMode(mode) || !validAutoMergeMode(expected) {
		return errors.New("invalid auto merge mode")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := tx.Exec(`UPDATE cards SET auto_merge_mode=? WHERE id=? AND auto_merge_mode=?`, mode, id, expected)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("auto merge preference changed; reload before saving")
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES('auto-merge:generation','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT)`); err != nil {
		return err
	}
	// Disabling is always permitted. A dispatched operation retains its outcome.
	if _, err = tx.Exec(`UPDATE auto_merge_operations SET state='deferred',category='policy_changed',next_check_at=0 WHERE card_id=? AND state NOT IN ('inflight','uncertain','merged')`, id); err != nil {
		return err
	}
	return tx.Commit()
}

type autoMergeWorker struct {
	mu      sync.Mutex
	adapter *mergeAdapter
}

func (w *Workflow) SetAutoMerger(s *Store) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.merges = &autoMergeWorker{adapter: newMergeAdapter(s, nil)}
}

// Token covers public policy/configuration only; credentials never enter storage.
func mergeConfig(q readyQuery, a Attempt) (token, provider, repo, base string, err error) {
	var mode, global string
	err = q.QueryRow(`SELECT c.auto_merge_mode,coalesce((SELECT value FROM settings WHERE key='dashboard:auto-merge'),'false'),p.provider,p.repo,p.base_branch FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, a.CardID).Scan(&mode, &global, &provider, &repo, &base)
	if err != nil {
		return
	}
	if global != "true" && global != "false" || !autoMergeEffective(global == "true", mode) {
		err = errors.New("auto merge disabled")
		return
	}
	var gen string
	err = q.QueryRow(`SELECT coalesce((SELECT value FROM settings WHERE key='auto-merge:generation'),'0')`).Scan(&gen)
	if err != nil {
		return
	}
	raw, _ := json.Marshal([]string{mode, global, gen, provider, repo, base, a.ID, a.PRURL, a.TaskJSON})
	sum := sha256.Sum256(raw)
	token = hex.EncodeToString(sum[:])
	return
}
func autoMergeEligible(tx *sql.Tx, a Attempt) error {
	var eligible bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id WHERE a.id=? AND a.pr_url=? AND a.ready=1 AND a.state='ready' AND a.error<>? AND c.status='Done' AND c.archived=0 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM workflow_ready_overrides WHERE attempt_id=a.id)
 AND NOT EXISTS(SELECT 1 FROM workflow_pr_merge_overrides m JOIN projects p ON p.id=c.project_id WHERE m.attempt_id=a.id AND m.pr_url=a.pr_url AND m.provider=p.provider AND m.repo=p.repo AND m.base_branch=p.base_branch))`, a.ID, a.PRURL, executionConflictNotice).Scan(&eligible)
	if err != nil {
		return err
	}
	if !eligible {
		return errors.New("auto merge no longer eligible")
	}
	return noActiveAttempts(tx, a.CardID)
}
func mergeBackoff(retries int) time.Duration {
	if retries > 5 {
		retries = 5
	}
	if retries < 0 {
		retries = 0
	}
	delay := time.Minute * time.Duration(1<<retries)
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	return delay
}

// A single bounded worker owns provider I/O. Rows are closed before using the
// Store's single connection; no write transaction spans a provider request.
func (w *Workflow) autoMergePass(ctx context.Context) {
	w.mu.RLock()
	worker := w.merges
	w.mu.RUnlock()
	if worker == nil || !worker.mu.TryLock() {
		return
	}
	defer worker.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := w.s.db.QueryContext(ctx, `SELECT id FROM (
 SELECT a.id, min(m.next_check_at) due FROM auto_merge_operations m JOIN workflow_attempts a ON a.id=m.attempt_id
 WHERE m.state IN ('reserved','inflight','uncertain') AND m.next_check_at<=? GROUP BY a.id
 UNION ALL
 SELECT a.id,coalesce((SELECT max(next_check_at) FROM auto_merge_operations WHERE attempt_id=a.id),0) due
 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id
 WHERE c.status='Done' AND c.archived=0 AND a.ready=1 AND a.state='ready'
 AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 AND a.error<>?
 AND NOT EXISTS(SELECT 1 FROM failed_resumes WHERE card_id=c.id AND status='pending')
 AND NOT EXISTS(SELECT 1 FROM workflow_attempts old WHERE old.card_id=c.id AND (old.state IN ('queued','submitted','running','waiting','interrupted') OR old.run_state IN ('queued','submitted','running','waiting','interrupted')))
 AND (c.auto_merge_mode='on' OR c.auto_merge_mode='inherit' AND coalesce((SELECT value FROM settings WHERE key='dashboard:auto-merge'),'false')='true')
 AND NOT EXISTS(SELECT 1 FROM workflow_ready_overrides WHERE attempt_id=a.id)
 AND NOT EXISTS(SELECT 1 FROM workflow_pr_merge_overrides m JOIN projects p ON p.id=c.project_id WHERE m.attempt_id=a.id AND m.pr_url=a.pr_url AND m.provider=p.provider AND m.repo=p.repo AND m.base_branch=p.base_branch)
 AND NOT EXISTS(SELECT 1 FROM auto_merge_operations m WHERE m.attempt_id=a.id AND m.state IN ('merged','reserved','inflight','uncertain'))
 ) WHERE due<=? GROUP BY id ORDER BY min(due),id LIMIT 16`, time.Now().UnixMilli(), executionConflictNotice, time.Now().UnixMilli())
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			break
		}
		ids = append(ids, id)
	}
	_ = rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		w.autoMergeOne(ctx, worker.adapter, id)
	}
}
func (w *Workflow) autoMergeOne(ctx context.Context, adapter *mergeAdapter, id string) {
	a, err := scanAttempt(w.s.db.QueryRowContext(ctx, `SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, id))
	if err != nil {
		return
	}
	// Always reconcile unresolved writes before considering a new head or policy.
	var op, sha, state string
	var retries int
	var due int64
	err = w.s.db.QueryRow(`SELECT id,head_sha,state,retries,next_check_at FROM auto_merge_operations WHERE attempt_id=? AND state IN ('reserved','inflight','uncertain') ORDER BY created_at LIMIT 1`, id).Scan(&op, &sha, &state, &retries, &due)
	if err == nil {
		if due > time.Now().UnixMilli() {
			return
		}
		w.finishMergeOperation(op, a, sha, retries, adapter.confirm(ctx, a, sha))
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	token, provider, repo, base, err := mergeConfig(w.s.db, a)
	if err != nil {
		return
	}
	// Throttle preparation failures as well as refused writes.
	var lastToken string
	err = w.s.db.QueryRow(`SELECT next_check_at,config_token FROM auto_merge_operations WHERE attempt_id=? ORDER BY updated_at DESC LIMIT 1`, id).Scan(&due, &lastToken)
	if err == nil && token == lastToken && due > time.Now().UnixMilli() {
		return
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return
	}
	err = autoMergeEligible(tx, a)
	_ = tx.Rollback()
	if err != nil {
		return
	}
	v, err := adapter.prepare(ctx, a)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		w.recordMergePreparation(a, token, provider, repo, base)
		return
	}
	now := time.Now().UnixMilli()
	tx, err = w.s.writeTx()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	current, _, _, _, err := mergeConfig(tx, a)
	if err != nil || current != token || autoMergeEligible(tx, a) != nil {
		return
	}
	var unresolved bool
	if tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM auto_merge_operations WHERE card_id=? AND state IN ('reserved','inflight','uncertain'))`, a.CardID).Scan(&unresolved) != nil || unresolved {
		return
	}
	op = newID()
	state = "reserved"
	if v.merged {
		state = "merged"
	}
	_, err = tx.Exec(`INSERT INTO auto_merge_operations(id,card_id,attempt_id,provider,repository,base_branch,pr_url,head_sha,config_token,state,next_check_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(attempt_id,provider,repository,base_branch,pr_url,head_sha) DO UPDATE SET state=excluded.state,config_token=excluded.config_token,updated_at=excluded.updated_at WHERE auto_merge_operations.state IN ('deferred','rejected')`, op, a.CardID, id, provider, repo, base, a.PRURL, v.sha, token, state, now, now, now)
	if err != nil {
		return
	}
	err = tx.QueryRow(`SELECT id,state,retries FROM auto_merge_operations WHERE attempt_id=? AND provider=? AND repository=? AND base_branch=? AND pr_url=? AND head_sha=?`, id, provider, repo, base, a.PRURL, v.sha).Scan(&op, &state, &retries)
	if err != nil || tx.Commit() != nil || state != "reserved" {
		return
	}
	// Last authorization check and durable dispatch marker share a transaction.
	dispatchTx, err := w.s.writeTx()
	if err != nil {
		return
	}
	current, _, _, _, err = mergeConfig(dispatchTx, a)
	if err == nil && current == token {
		err = autoMergeEligible(dispatchTx, a)
	} else {
		err = errors.New("policy changed")
	}
	if err != nil {
		_, _ = dispatchTx.Exec(`UPDATE auto_merge_operations SET state='deferred',category='policy_changed' WHERE id=? AND state='reserved'`, op)
		_ = dispatchTx.Commit()
		return
	}
	res, err := dispatchTx.Exec(`UPDATE auto_merge_operations SET state='inflight' WHERE id=? AND state='reserved' AND config_token=?`, op, token)
	if err != nil {
		_ = dispatchTx.Rollback()
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 || dispatchTx.Commit() != nil {
		_ = dispatchTx.Rollback()
		return
	}
	w.finishMergeOperation(op, a, v.sha, retries, adapter.dispatch(ctx, a, v))
}
func (w *Workflow) recordMergePreparation(a Attempt, token, provider, repo, base string) {
	now := time.Now().UnixMilli()
	var retries int
	_ = w.s.db.QueryRow(`SELECT retries FROM auto_merge_operations WHERE attempt_id=? AND provider=? AND repository=? AND base_branch=? AND pr_url=? AND head_sha=''`, a.ID, provider, repo, base, a.PRURL).Scan(&retries)
	_, _ = w.s.db.Exec(`INSERT INTO auto_merge_operations(id,card_id,attempt_id,provider,repository,base_branch,pr_url,head_sha,config_token,state,category,next_check_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'',?,'deferred','readiness_pending',?,?,?) ON CONFLICT(attempt_id,provider,repository,base_branch,pr_url,head_sha) DO UPDATE SET config_token=excluded.config_token,next_check_at=excluded.next_check_at,updated_at=excluded.updated_at,retries=auto_merge_operations.retries+1`, newID(), a.CardID, a.ID, provider, repo, base, a.PRURL, token, now+mergeBackoff(retries).Milliseconds(), now, now)
}
func (w *Workflow) finishMergeOperation(op string, a Attempt, sha string, retries int, out autoMergeOutcome) {
	now := time.Now()
	due := now.Add(mergeBackoff(retries))
	if out.retryAfter.After(due) {
		due = out.retryAfter
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`UPDATE auto_merge_operations SET state=?,category=?,retries=retries+1,next_check_at=?,updated_at=? WHERE id=? AND state IN ('reserved','inflight','uncertain')`, out.state, out.category, due.UnixMilli(), now.UnixMilli(), op)
	if err != nil {
		return
	}
	if out.state == "merged" {
		_, err = tx.Exec(`INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,head_sha,checked_at) SELECT a.id,a.pr_url,p.provider,p.repo,p.base_branch,'merged',?,? FROM workflow_attempts a JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id JOIN auto_merge_operations m ON m.id=? WHERE a.id=? AND a.pr_url=m.pr_url AND p.provider=m.provider AND p.repo=m.repository AND p.base_branch=m.base_branch ON CONFLICT(attempt_id) DO UPDATE SET state='merged',head_sha=excluded.head_sha,checked_at=excluded.checked_at`, sha, now.UnixMilli(), op, a.ID)
	}
	if err == nil {
		_ = tx.Commit()
	}
}

type autoMergeStatus struct {
	State, Category string
	UpdatedAt       time.Time
}

func (s *Store) autoMergeStatuses() (map[string]autoMergeStatus, error) {
	rows, err := s.db.Query(`SELECT m.card_id,m.state,m.category,m.updated_at FROM auto_merge_operations m JOIN workflow_attempts a ON a.id=m.attempt_id JOIN cards c ON c.id=a.card_id JOIN projects p ON p.id=c.project_id WHERE a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id) AND m.pr_url=a.pr_url AND m.provider=p.provider AND m.repository=p.repo AND m.base_branch=p.base_branch ORDER BY m.updated_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]autoMergeStatus{}
	for rows.Next() {
		var card string
		var status autoMergeStatus
		var ms int64
		if err = rows.Scan(&card, &status.State, &status.Category, &ms); err != nil {
			return nil, err
		}
		status.UpdatedAt = time.UnixMilli(ms)
		out[card] = status
	}
	return out, rows.Err()
}
func autoMergeSavedLabel(mode string, global bool) string {
	switch mode {
	case "inherit":
		if global {
			return "Use global · effective On"
		}
		return "Use global · effective Off"
	case "on":
		if !global {
			return "On · overrides global Off"
		}
		return "On"
	case "off":
		return "Off · never auto merge this card"
	}
	return "Invalid preference · auto merge disabled"
}
