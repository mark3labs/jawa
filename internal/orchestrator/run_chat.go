package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/orchestrator/db/sqlc"
	"jawa/internal/reportprotocol"
	"jawa/internal/runapitoken"
)

// RunChatMessage is durable operator input, not workflow completion evidence.
type RunChatMessage struct {
	ID, ClientID, Mode, LocalID, CardID, TaskID, AgentID, RunID, AttemptID string
	Text, Classification, Interaction, Status, Failure, Request, Receipt   string
	CreatedAt                                                              int64
	InTranscript                                                           bool
}

const chatColumns = `id,client_id,mode,local_id,card_id,task_id,agent_id,run_id,attempt_id,text,classification,interaction,status,failure,request_json,receipt_json,created_at`

func scanChat(row interface{ Scan(...any) error }) (m RunChatMessage, err error) {
	err = row.Scan(&m.ID, &m.ClientID, &m.Mode, &m.LocalID, &m.CardID, &m.TaskID, &m.AgentID, &m.RunID, &m.AttemptID, &m.Text, &m.Classification, &m.Interaction, &m.Status, &m.Failure, &m.Request, &m.Receipt, &m.CreatedAt)
	return
}

// resolveChat uses the same transaction as admission and never accepts remote
// identity from browser input. Historical records remain independently pinned.
func resolveChat(ctx context.Context, tx *sql.Tx, mode, id string) (RunRecord, error) {
	if mode == "Planning" {
		t, e := sqlc.New(tx).GetPlanningRunHistory(ctx, id)
		return planningRun(sqlc.ListPlanningRunHistoryRow(t)), e
	}
	a, e := scanAttempt(tx.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, id))
	return buildRun(a), e
}
func classifyChat(tx *sql.Tx, r RunRecord) (string, error) {
	var current bool
	reported, err := acceptedReport(tx, r.TaskID)
	if err != nil {
		return "", err
	}
	if reported {
		return "followup", nil
	}

	if r.Mode == "Planning" {
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id JOIN cards c ON c.id=s.card_id WHERE t.id=? AND c.status='Planning' AND s.id=(SELECT id FROM planning_sessions WHERE card_id=c.id ORDER BY created_at DESC,rowid DESC LIMIT 1) AND s.state NOT IN ('approved','cancelled') AND t.state<>'cancelled' AND t.number=(SELECT max(number) FROM planning_turns WHERE session_id=s.id))`, r.ID).Scan(&current)
	} else {
		err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM workflow_attempts a JOIN cards c ON c.id=a.card_id WHERE a.id=? AND c.status IN ('Building','Fix') AND a.ready=0 AND a.run_state<>'abandoned' AND a.number=(SELECT max(number) FROM workflow_attempts WHERE card_id=c.id) AND NOT EXISTS(SELECT 1 FROM typed_reports x WHERE x.task_id=a.task_id AND x.kind='complete_task'))`, r.ID).Scan(&current)
	}
	if err != nil {
		return "", err
	}
	if current {
		return "continuation", nil
	}
	return "followup", nil
}
func chatInteraction(tx *sql.Tx, r RunRecord) (token, status string, err error) {
	table := "workflow_interactions"
	if r.Mode == "Planning" {
		table = "planning_interactions"
	}
	err = tx.QueryRow(`SELECT report_id,status FROM `+table+` WHERE task_id=? AND status IN ('pending','queued')`, r.TaskID).Scan(&token, &status)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

// QueueRunChat commits acceptance before any network I/O. Replayed client IDs
// bind exact text and displayed interaction; conflicting reuse never executes.
func (w *Workflow) QueueRunChat(ctx context.Context, mode, id, clientID, text, interaction string) (RunChatMessage, error) {
	if strings.TrimSpace(text) == "" || !utf8.ValidString(text) || len(text) > maxGuidanceBytes {
		return RunChatMessage{}, errors.New("enter a message of 1 to 64000 UTF-8 bytes")
	}
	if !validAttemptID(clientID) {
		return RunChatMessage{}, errors.New("missing message identity; reload before sending")
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return RunChatMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	r, err := resolveChat(ctx, tx, mode, id)
	if err != nil || !r.pinned() {
		return RunChatMessage{}, errors.New("run identity unavailable")
	}
	old, err := scanChat(tx.QueryRow(`SELECT `+chatColumns+` FROM run_chat WHERE mode=? AND local_id=? AND client_id=?`, r.Mode, id, clientID))
	if err == nil {
		if old.Text != text || old.Interaction != interaction {
			return old, errors.New("message identity already used with different content")
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return old, err
	}
	class, err := classifyChat(tx, r)
	if err != nil {
		return old, err
	}
	now := time.Now().UnixMilli()
	m := RunChatMessage{ID: uuid.NewString(), ClientID: clientID, Mode: r.Mode, LocalID: r.ID, CardID: r.CardID, TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.RemoteAttemptID, Text: text, Classification: class, Interaction: interaction, Status: "queued", CreatedAt: now}
	_, err = tx.Exec(`INSERT INTO run_chat(id,client_id,mode,local_id,card_id,task_id,agent_id,run_id,attempt_id,text,classification,interaction,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, clientID, r.Mode, id, r.CardID, r.TaskID, r.AgentID, r.RunID, r.RemoteAttemptID, text, class, interaction, now, now)
	if err != nil {
		return m, err
	}
	return m, tx.Commit()
}
func fromSQLChat(row sqlc.RunChat) RunChatMessage {
	return RunChatMessage{ID: row.ID, ClientID: row.ClientID, Mode: row.Mode, LocalID: row.LocalID, CardID: row.CardID, TaskID: row.TaskID, AgentID: row.AgentID, RunID: row.RunID, AttemptID: row.AttemptID, Text: row.Text, Classification: row.Classification, Interaction: row.Interaction, Status: row.Status, Failure: row.Failure, Request: row.RequestJson, Receipt: row.ReceiptJson, CreatedAt: row.CreatedAt}
}
func (w *Workflow) chatMessages(r RunRecord) []RunChatMessage {
	rows, err := sqlc.New(w.s.db).ListRunChatHistory(w.ctx, sqlc.ListRunChatHistoryParams{Mode: r.Mode, LocalID: r.ID})
	if err != nil {
		return nil
	}
	messages := make([]RunChatMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, fromSQLChat(row))
	}
	return messages
}

// Follow-up commands permanently fence later workflow evidence for that task.
// Continuations suppress transport projections while their receipt is pending;
// typed reports remain independently validated and authoritative.
func chatEvidenceBlocked(tx *sql.Tx, task string) (bool, error) {
	var blocked bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_chat WHERE task_id=? AND request_json<>'' AND (classification='followup' OR status IN ('dispatched','active','ambiguous')))`, task).Scan(&blocked)
	return blocked, err
}
func chatContinuation(tx *sql.Tx, task string) (bool, error) {
	var active bool
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_chat WHERE task_id=? AND classification='continuation' AND status IN ('dispatched','active') AND request_json<>'')`, task).Scan(&active)
	return active, err
}
func noActiveChat(tx *sql.Tx, card string) error {
	var pending bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM run_chat WHERE card_id=? AND status IN ('dispatched','active','ambiguous'))`, card).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return errors.New("chat delivery is active or unconfirmed; wait for its owning agent")
	}
	return nil
}
func (w *Workflow) failChat(id, code string) {
	_, _ = w.s.db.Exec(`UPDATE run_chat SET status='failed',failure=?,updated_at=? WHERE id=? AND status='queued'`, code, time.Now().UnixMilli(), id)
}

// Bounded, ordered passes retain one admitted command per run. Transport retry
// preserves command identity and never allocates a fresh workflow attempt.
func (w *Workflow) dispatchRunChat() {
	if w.client == nil || w.runAPI == nil {
		return
	}
	rows, err := sqlc.New(w.s.db).ListPendingRunChat(w.ctx)
	if err != nil {
		return
	}
	messages := make([]RunChatMessage, 0, len(rows))
	for _, row := range rows {
		messages = append(messages, fromSQLChat(row))
	}
	pass, cancel := context.WithTimeout(w.ctx, 20*time.Second)
	defer cancel()
	for _, m := range messages {
		if pass.Err() != nil {
			return
		}
		w.dispatchChatMessage(pass, m)
	}
}
func (w *Workflow) dispatchChatMessage(ctx context.Context, m RunChatMessage) {
	var cmd reportprotocol.Chat
	if m.Classification == "interaction" {
		return
	}
	if m.Request == "" {
		tx, err := w.s.writeTx()
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		r, err := resolveChat(ctx, tx, m.Mode, m.LocalID)
		if err != nil || r.AgentID != m.AgentID || r.RunID != m.RunID || r.RemoteAttemptID != m.AttemptID {
			_ = tx.Rollback()
			w.failChat(m.ID, "identity_changed")
			return
		}
		class, err := classifyChat(tx, r)
		if err != nil {
			return
		}
		if m.Classification == "followup" {
			class = "followup"
		}
		var pending bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM failed_resumes WHERE task_id=? AND status='pending') OR EXISTS(SELECT 1 FROM workflow_cancel_requests WHERE id=(SELECT max(id) FROM workflow_cancel_requests WHERE task_id=?) AND status IN ('pending','requested','uncertain'))`, r.TaskID, r.TaskID).Scan(&pending); err != nil || pending {
			return
		}
		token, state, err := chatInteraction(tx, r)
		if err != nil {
			return
		}
		if class == "continuation" && token != "" {
			if state == "queued" {
				return
			}
			if token != m.Interaction {
				_ = tx.Rollback()
				w.failChat(m.ID, "interaction_changed")
				return
			}
			if r.Mode == "Planning" {
				var session string
				if err = tx.QueryRow(`SELECT session_id FROM planning_turns WHERE id=?`, r.ID).Scan(&session); err != nil {
					return
				}
				err = queuePlanningReply(tx, session, token, m.Text, false)
			} else {
				err = queueBlockedReply(tx, r.Build, token, m.Text)
			}
			if err != nil {
				return
			}
			_, err = tx.Exec(`UPDATE run_chat SET classification='interaction',status='active',updated_at=? WHERE id=? AND status='queued'`, time.Now().UnixMilli(), m.ID)
			if err == nil {
				_ = tx.Commit()
			}
			return
		}
		target := client.Target{TaskID: r.TaskID, AgentID: r.AgentID, RunID: r.RunID, AttemptID: r.RemoteAttemptID}
		query, cancel := context.WithTimeout(ctx, time.Second)
		status, e := w.client.Status(query, target)
		cancel()
		if e != nil || status.Target != target {
			_, _ = tx.Exec(`UPDATE run_chat SET failure='owner_unreachable_or_journal_unavailable' WHERE id=?`, m.ID)
			_ = tx.Commit()
			return
		}
		if status.Active && class == "followup" {
			return
		}
		cmd = reportprotocol.Chat{Version: 1, ID: m.ID, Target: target, Text: m.Text, Followup: class == "followup", Seq: status.Seq, TurnID: status.TurnID}
		raw, _ := json.Marshal(cmd)
		_, err = tx.Exec(`UPDATE run_chat SET classification=?,status='dispatched',request_json=?,failure='',updated_at=? WHERE id=? AND status='queued'`, class, string(raw), time.Now().UnixMilli(), m.ID)
		if err != nil || tx.Commit() != nil {
			return
		}
	} else if json.Unmarshal([]byte(m.Request), &cmd) != nil {
		return
	}
	if m.Classification == "interaction" {
		return
	}
	receipt, err := w.sendChat(ctx, cmd, 2*time.Second)
	if err != nil {
		_, _ = w.s.db.Exec(`UPDATE run_chat SET failure='owner_unreachable_or_unsupported' WHERE id=?`, m.ID)
		return
	} // Offline/old agents remain visibly pending, never fallback.
	w.applyChatReceipt(ctx, m, cmd, receipt)
}

// applyChatReceipt projects only host-confirmed execution into the existing workflow.
func (w *Workflow) applyChatReceipt(ctx context.Context, m RunChatMessage, cmd reportprotocol.Chat, receipt reportprotocol.ChatReceipt) {
	tx, err := w.s.writeTx()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	raw, _ := json.Marshal(receipt)
	switch receipt.Status {
	case "queued":
		// Host explicitly confirms no admission; refreshing a cursor is safe only
		// here, never after a timeout or ambiguous intent.
		_, err = tx.Exec(`UPDATE run_chat SET status='queued',request_json='',failure=?,updated_at=? WHERE id=?`, receipt.Failure, time.Now().UnixMilli(), m.ID)
	case "active", "completed", "failed", "ambiguous":
		_, err = tx.Exec(`UPDATE run_chat SET status=?,failure=?,receipt_json=?,updated_at=? WHERE id=?`, receipt.Status, receipt.Failure, string(raw), time.Now().UnixMilli(), m.ID)
	default:
		return
	}
	if err != nil {
		return
	}
	if receipt.Status == "active" && !cmd.Followup {
		r, e := resolveChat(ctx, tx, m.Mode, m.LocalID)
		if e != nil {
			return
		}
		class, e := classifyChat(tx, r)
		if e != nil {
			return
		}
		if class == "continuation" {
			// Host confirmation revives only failed work, never a newer typed interaction.
			if m.Mode == "Planning" {
				result, e := tx.Exec(`UPDATE planning_turns SET state='running',outcome_json='',error='',updated_at=? WHERE id=? AND state='failed' AND NOT EXISTS(SELECT 1 FROM planning_interactions WHERE task_id=? AND status IN ('pending','queued'))`, time.Now().UnixMilli(), m.LocalID, m.TaskID)
				if e != nil {
					return
				}
				changed, e := result.RowsAffected()
				if e != nil {
					return
				}
				if changed > 0 {
					_, err = tx.Exec(`UPDATE planning_sessions SET state='running',error='',updated_at=? WHERE id=(SELECT session_id FROM planning_turns WHERE id=?) AND state='failed'`, time.Now().UnixMilli(), m.LocalID)
				}
			} else {
				_, err = tx.Exec(`UPDATE workflow_attempts SET state='running',run_state='running',outcome_json='',error='',updated_at=? WHERE id=? AND ready=0 AND state='failed' AND NOT EXISTS(SELECT 1 FROM workflow_interactions WHERE task_id=workflow_attempts.task_id AND status IN ('pending','queued'))`, time.Now().UnixMilli(), m.LocalID)
			}
		}
	}

	if receipt.Status == "completed" && !cmd.Followup && receipt.Outcome != nil {
		reported, e := acceptedReport(tx, m.TaskID)
		if e != nil {
			return
		}
		if !reported {
			r, e := resolveChat(ctx, tx, m.Mode, m.LocalID)
			if e != nil {
				return
			}
			class, e := classifyChat(tx, r)
			if e != nil {
				return
			}
			if class == "continuation" {
				if m.Mode == "Planning" {
					var pending bool
					if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM planning_interactions WHERE task_id=? AND status IN ('pending','queued'))`, m.TaskID).Scan(&pending); err == nil {
						if pending && receipt.Outcome.State == runtime.RunWaiting {
							// Retain the typed report's ready/questions projection and only
							// clear stale natural failure evidence so the exact trusted
							// suspension can be attached to its still-open interaction.
							_, err = tx.Exec(`UPDATE planning_turns SET outcome_json='' WHERE id=? AND state IN ('waiting','running','failed')`, m.TaskID)
							if err == nil {
								_, err = w.planningResultDelivery(tx, ctx, *receipt.Outcome, true)
							}
						} else if !pending {
							// Host confirms execution. Remove only superseded failure evidence;
							// ordinary result projection can then record a fresh natural failure.
							_, err = tx.Exec(`UPDATE planning_turns SET outcome_json='',state='running',error='' WHERE id=? AND state NOT IN ('cancelled','completed')`, m.TaskID)
							if err == nil {
								_, err = w.planningResultDelivery(tx, ctx, *receipt.Outcome, true)
							}
						}
					}
				} else {
					data, _ := json.Marshal(receipt.Outcome)
					handled, e := blockerOutcome(tx, r.Build, *receipt.Outcome, string(data))
					err = e
					if err == nil && handled {
						_, err = tx.Exec(`UPDATE workflow_attempts SET outcome_json=? WHERE id=?`, string(data), m.LocalID)
					}
					if err == nil && !handled {
						err = applyOutcome(tx, r.Build, *receipt.Outcome)
					}
				}
			}
		}
	}
	if err == nil && receipt.Status == "completed" && !cmd.Followup && receipt.Outcome != nil {
		table := "workflow_attempts"
		if m.Mode == "Planning" {
			table = "planning_turns"
		}
		_, err = tx.Exec(`UPDATE `+table+` SET event_seq=max(event_seq,?) WHERE id=?`, receipt.Seq, m.LocalID)
	}
	if err == nil {
		_ = tx.Commit()
	}
}

// Interaction delivery uses the existing exact Answer outboxes, not Chat.
func (w *Workflow) reconcileChatInteractions() {
	_, _ = w.s.db.Exec(`UPDATE run_chat SET status=CASE WHEN EXISTS(SELECT 1 FROM workflow_interactions WHERE report_id=interaction AND status='cancelled') OR EXISTS(SELECT 1 FROM planning_interactions WHERE report_id=interaction AND status='cancelled') THEN 'failed' ELSE 'completed' END,updated_at=? WHERE classification='interaction' AND status='active' AND (EXISTS(SELECT 1 FROM workflow_interactions WHERE report_id=interaction AND status IN ('answered','cancelled')) OR EXISTS(SELECT 1 FROM planning_interactions WHERE report_id=interaction AND status IN ('answered','cancelled')))`, time.Now().UnixMilli())
}

var (
	// ErrRunChatChanged means reconciliation lost a race with another resolution.
	ErrRunChatChanged           = errors.New("run chat message changed")
	errChatCommandUnavailable   = errors.New("chat command unavailable")
	errChatReconcileUnconfirmed = errors.New("chat reconciliation unconfirmed")
)

func (w *Workflow) sendChat(ctx context.Context, cmd reportprotocol.Chat, timeout time.Duration) (reportprotocol.ChatReceipt, error) {
	if w.runAPI == nil {
		return reportprotocol.ChatReceipt{}, errChatCommandUnavailable
	}
	cmd.Token = runapitoken.Sign(w.runAPI.key, cmd.Target.AgentID, time.Now())
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return reportprotocol.SendChat(call, w.runAPI.conn, cmd)
}

// ReconcileRunChat fences the original command without replaying uncertain work.
// Network I/O precedes the write transaction; the conditional update detects any
// resolution committed while the owner was being queried.
func (w *Workflow) ReconcileRunChat(ctx context.Context, record RunRecord, chatID string) error {
	m, err := scanChat(w.s.db.QueryRowContext(ctx, `SELECT `+chatColumns+` FROM run_chat WHERE id=? AND mode=? AND local_id=? AND status='ambiguous'`, chatID, record.Mode, record.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRunChatChanged
	}
	if err != nil {
		return err
	}
	var cmd reportprotocol.Chat
	if json.Unmarshal([]byte(m.Request), &cmd) != nil {
		return errChatCommandUnavailable
	}
	cmd.Reconcile = true
	receipt, err := w.sendChat(ctx, cmd, 5*time.Second)
	if err != nil || receipt.Status != "failed" || receipt.Failure != "reconciled_not_replayed" {
		return errChatReconcileUnconfirmed
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE run_chat SET status='failed',failure=?,receipt_json=?,updated_at=? WHERE id=? AND status='ambiguous'`, receipt.Failure, string(raw), time.Now().UnixMilli(), m.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrRunChatChanged
	}
	return tx.Commit()
}
