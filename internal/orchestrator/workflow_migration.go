package orchestrator

import "database/sql"

func migrateWorkflow(s *Store) error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS workflow_attempts (
 id TEXT PRIMARY KEY, card_id TEXT NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
 number INTEGER NOT NULL, task_id TEXT NOT NULL UNIQUE, task_json TEXT NOT NULL,
 agent_id TEXT NOT NULL DEFAULT '', run_id TEXT NOT NULL DEFAULT '', remote_attempt_id TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'queued', run_state TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
 outcome_json TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '', pr_url TEXT NOT NULL DEFAULT '',
 published INTEGER NOT NULL DEFAULT 0, ready INTEGER NOT NULL DEFAULT 0, event_seq INTEGER NOT NULL DEFAULT -1,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, UNIQUE(card_id,number));
 CREATE TABLE IF NOT EXISTS workflow_conflicts (
 id INTEGER PRIMARY KEY, task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL, run_id TEXT NOT NULL, remote_attempt_id TEXT NOT NULL,
 outcome_json TEXT NOT NULL, received_at INTEGER NOT NULL, reason TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS workflow_conflicts_identity ON workflow_conflicts(task_id,run_id,remote_attempt_id);
 CREATE TABLE IF NOT EXISTS workflow_execution_events (
 task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 event_id TEXT NOT NULL, agent_id TEXT NOT NULL, run_id TEXT NOT NULL, remote_attempt_id TEXT NOT NULL,
 event_type TEXT NOT NULL, event_json TEXT NOT NULL, received_at INTEGER NOT NULL,
 PRIMARY KEY(task_id,event_id));
 CREATE TABLE IF NOT EXISTS workflow_reconciliations (
 id INTEGER PRIMARY KEY, task_id TEXT NOT NULL REFERENCES workflow_attempts(task_id) ON DELETE CASCADE,
 previous_agent_id TEXT NOT NULL, previous_run_id TEXT NOT NULL, previous_remote_attempt_id TEXT NOT NULL,
 selected_agent_id TEXT NOT NULL, selected_run_id TEXT NOT NULL, selected_remote_attempt_id TEXT NOT NULL,
 conflict_id INTEGER NOT NULL REFERENCES workflow_conflicts(id) ON DELETE CASCADE,
 reconciled_at INTEGER NOT NULL);`)
	if err != nil {
		return err
	}
	// Rename persisted v0.20 columns once; all runtime queries use Agent naming.
	for _, column := range []struct{ table, old, name string }{
		{"workflow_attempts", "worker_id", "agent_id"},
		{"workflow_conflicts", "worker_id", "agent_id"},
		{"workflow_execution_events", "worker_id", "agent_id"},
		{"workflow_reconciliations", "previous_worker_id", "previous_agent_id"},
		{"workflow_reconciliations", "selected_worker_id", "selected_agent_id"},
	} {
		var exists bool
		if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)`, column.table, column.old).Scan(&exists); err != nil {
			return err
		}
		if exists {
			if _, err := s.db.Exec(`ALTER TABLE ` + column.table + ` RENAME COLUMN ` + column.old + ` TO ` + column.name); err != nil {
				return err
			}
		}
	}
	return nil
}

// Keep the Store's stable lane ordering while using the caller's transaction.
func moveWorkflowCard(tx *sql.Tx, id, project, source, status string, position int) error {
	// Rebuild both affected lanes in their stable order. Excluding the moving
	// card first makes position mean its final zero-based index in all cases.
	lanes := []string{source}
	if source != status {
		lanes = append(lanes, status)
	}
	for _, lane := range lanes {
		rows, err := tx.Query(`SELECT id FROM cards WHERE project_id=? AND status=? AND id<>? ORDER BY position,created_at,id`, project, lane, id)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var cardID string
			if err = rows.Scan(&cardID); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, cardID)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if lane == status {
			pos := min(position, len(ids))
			ids = append(ids, "")
			copy(ids[pos+1:], ids[pos:])
			ids[pos] = id
		}
		for i, cardID := range ids {
			if _, err = tx.Exec(`UPDATE cards SET status=?,position=? WHERE id=? AND project_id=?`, lane, i, cardID, project); err != nil {
				return err
			}
		}
	}
	return nil
}
