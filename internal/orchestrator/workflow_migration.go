package orchestrator

import "database/sql"

func migrateWorkflow(s *Store) error {
	// OpenStore owns all schema initialization. Keep this compatibility hook
	// for workflow callers without running a second migration pass.
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
