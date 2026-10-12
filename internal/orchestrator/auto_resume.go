package orchestrator

import (
	"database/sql"
	"errors"
	"time"
)

type autoResumeQuerier interface{ QueryRow(string, ...any) *sql.Row }

// Invalid persisted values fail closed, including during reservation.
func autoResumeEnabled(q autoResumeQuerier) (bool, error) {
	var value string
	err := q.QueryRow(`SELECT value FROM settings WHERE key='dashboard:auto-resume-failed-runs'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, errors.New("invalid automatic resume setting")
}

func autoResumeDelay(count int) time.Duration { return 5 * time.Second * time.Duration(1<<count) }

// Discovery is only a scheduling hint. The shared reservation rechecks the
// current execution, owner and deadline in its write transaction.
func (w *Workflow) discoverAutoResumes(now time.Time) error {
	// Retired tasks must not occupy the bounded due batch indefinitely.
	if _, err := w.s.db.Exec(`DELETE FROM auto_resumes WHERE state<>'reserved' AND NOT EXISTS (
 SELECT 1 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id WHERE a.task_id=auto_resumes.task_id AND c.status IN ('Building','Fix') AND a.state='failed' AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id)
 UNION ALL
 SELECT 1 FROM planning_turns t JOIN planning_sessions p ON p.id=t.session_id JOIN cards c ON c.id=p.card_id WHERE t.id=auto_resumes.task_id AND c.status='Planning' AND p.state='failed' AND t.state='failed' AND p.id=(SELECT id FROM planning_sessions WHERE card_id=c.id ORDER BY created_at DESC LIMIT 1) AND t.number=(SELECT max(number) FROM planning_turns WHERE session_id=p.id))`); err != nil {
		return err
	}

	rows, err := w.s.db.Query(`SELECT a.card_id,0,a.task_id,a.state,a.run_state,a.agent_id,a.run_id,a.remote_attempt_id,a.outcome_json,a.error,a.event_seq,a.ready
 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id
 WHERE c.status IN ('Building','Fix') AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=a.card_id) AND a.state='failed'
 UNION ALL
 SELECT p.card_id,1,t.id,t.state,'failed',t.agent_id,t.run_id,t.remote_attempt_id,t.outcome_json,t.error,t.event_seq,0
 FROM planning_turns t JOIN planning_sessions p ON p.id=t.session_id JOIN cards c ON c.id=p.card_id
 WHERE c.status='Planning' AND p.state='failed' AND t.state='failed'
 AND p.id=(SELECT id FROM planning_sessions WHERE card_id=p.card_id ORDER BY created_at DESC LIMIT 1)
 AND t.number=(SELECT max(number) FROM planning_turns WHERE session_id=p.id)`)
	if err != nil {
		return err
	}
	type candidate struct {
		a        Attempt
		planning bool
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.a.CardID, &c.planning, &c.a.TaskID, &c.a.State, &c.a.RunState, &c.a.AgentID, &c.a.RunID, &c.a.RemoteAttemptID, &c.a.OutcomeJSON, &c.a.Error, &c.a.EventSeq, &c.a.Ready); err != nil {
			break
		}
		if executionFailure(c.a) {
			candidates = append(candidates, c)
		}
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, c := range candidates {
		tx, e := w.s.writeTx()
		if e != nil {
			return e
		}
		// A receipt/manual action may have committed since discovery. Never replace
		// its stopped/reserved generation using an older snapshot.
		var current Attempt
		if c.planning {
			current.TaskID = c.a.TaskID
			e = tx.QueryRow(`SELECT outcome_json,event_seq FROM planning_turns WHERE id=?`, c.a.TaskID).Scan(&current.OutcomeJSON, &current.EventSeq)
		} else {
			current, e = scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, c.a.TaskID))
		}
		if e != nil || resumeToken(current) != resumeToken(c.a) {
			_ = tx.Rollback()
			continue
		}
		var excluded bool
		e = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM typed_reports WHERE task_id=? AND kind IN ('complete_task','report_blocker')) OR EXISTS(SELECT 1 FROM failed_resumes WHERE card_id=? AND status='pending')`, c.a.TaskID, c.a.CardID).Scan(&excluded)
		var count int
		if e == nil && !excluded {
			e = tx.QueryRow(`SELECT count(*) FROM failed_resumes WHERE task_id=? AND automatic=1`, c.a.TaskID).Scan(&count)
		}
		if e == nil && !excluded {
			state := "scheduled"
			if count >= 5 {
				state = "exhausted"
			}
			due := now.Add(autoResumeDelay(count)).UnixMilli()
			_, e = tx.Exec(`INSERT INTO auto_resumes(task_id,card_id,planning,failure_token,due_at,next_check_at,state) VALUES(?,?,?,?,?,?,?)
    ON CONFLICT(task_id) DO UPDATE SET failure_token=excluded.failure_token,due_at=excluded.due_at,next_check_at=excluded.next_check_at,state=excluded.state
    WHERE auto_resumes.failure_token<>excluded.failure_token`, c.a.TaskID, c.a.CardID, c.planning, resumeToken(c.a), due, due, state)
		}
		if e == nil {
			e = tx.Commit()
		}
		_ = tx.Rollback()
		if e != nil {
			return e
		}
	}
	return nil
}

// No sleeping workers: persisted next_check_at throttles absent owners and
// survives restart. Pending commands are dispatched independently of the toggle.
func (w *Workflow) autoResumePass(now time.Time) {
	if w.ctx.Err() != nil {
		return
	}
	enabled, err := autoResumeEnabled(w.s.db)
	if err != nil || !enabled {
		return
	}
	if w.discoverAutoResumes(now) != nil {
		return
	}
	rows, err := w.s.db.Query(`SELECT task_id,card_id,planning,failure_token FROM auto_resumes WHERE state='scheduled' AND next_check_at<=? ORDER BY next_check_at,task_id LIMIT 25`, now.UnixMilli())
	if err != nil {
		return
	}
	type due struct {
		task, card, token string
		planning          bool
	}
	var batch []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.task, &d.card, &d.planning, &d.token) != nil {
			break
		}
		batch = append(batch, d)
	}
	_ = rows.Close()
	for _, d := range batch {
		if w.ctx.Err() != nil {
			return
		}
		if err = w.reserveFailedResume(d.card, d.planning, d.token, true, now); err != nil {
			retryAt := now
			if actual := time.Now(); actual.After(retryAt) {
				retryAt = actual
			}
			_, _ = w.s.db.Exec(`UPDATE auto_resumes SET next_check_at=? WHERE task_id=? AND failure_token=? AND state='scheduled'`, retryAt.Add(5*time.Second).UnixMilli(), d.task, d.token)
		}
	}
}

// Load status once into the snapshot used by native and SSE rendering. Obsolete
// schedules are hidden by comparing against the current failed execution token.
func (s *Store) autoResumeStatuses() (map[string]autoResumeStatus, error) {
	rows, err := s.db.Query(`SELECT task_id,failure_token,state,due_at,(SELECT count(*) FROM failed_resumes f WHERE f.task_id=a.task_id AND automatic=1) FROM auto_resumes a`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[string]autoResumeStatus{}
	for rows.Next() {
		var task string
		var status autoResumeStatus
		if err = rows.Scan(&task, &status.token, &status.state, &status.due, &status.count); err != nil {
			return nil, err
		}
		result[task] = status
	}
	return result, rows.Err()
}

type autoResumeStatus struct {
	token, state string
	due          int64
	count        int
}
