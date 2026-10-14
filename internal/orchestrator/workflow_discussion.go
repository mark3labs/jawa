package orchestrator

import (
	"encoding/json"

	"jawa/internal/reportprotocol"
)

// WorkflowExchange is read-only history, never authority to answer a blocker.
// Replies stay paired with their report; no reply timestamp is inferred.
type WorkflowExchange struct {
	CardID, TaskID, AttemptID, ReportID, Reason, Reply, Status string
	Number                                                     int
}

func (s *Store) workflowDiscussion(cardID string) ([]WorkflowExchange, error) {
	rows, err := s.db.Query(`SELECT a.card_id,a.task_id,a.id,a.number,r.report_id,r.request_json,COALESCE(i.reply,''),COALESCE(i.status,'')
 FROM typed_reports r JOIN workflow_attempts a ON a.task_id=r.task_id
 LEFT JOIN workflow_interactions i ON i.report_id=r.report_id
 WHERE a.card_id=? AND r.kind='report_blocker'
 ORDER BY a.number,r.accepted_at,r.rowid`, cardID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []WorkflowExchange
	for rows.Next() {
		var e WorkflowExchange
		var raw string
		if err = rows.Scan(&e.CardID, &e.TaskID, &e.AttemptID, &e.Number, &e.ReportID, &raw, &e.Reply, &e.Status); err != nil {
			return nil, err
		}
		var request reportprotocol.Request
		if err = json.Unmarshal([]byte(raw), &request); err != nil {
			return nil, err
		}
		e.Reason = request.Payload.Reason
		result = append(result, e)
	}
	return result, rows.Err()
}
