package orchestrator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
	"jawa/internal/runapitoken"
)

func TestReconcileRunChat(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "changed"}[concurrent], func(t *testing.T) {
			a := testApp(t)
			session, csrf := setupTest(t, a)
			attempt := pinnedDetailAttempt(t, a)
			record := buildRun(attempt)
			m, err := a.workflow.QueueRunChat(t.Context(), record.Mode, record.ID, uuid.NewString(), "hello", "")
			if err != nil {
				t.Fatal(err)
			}
			cmd := reportprotocol.Chat{Version: 1, ID: m.ID, Target: client.Target{TaskID: attempt.TaskID, AgentID: attempt.AgentID, RunID: attempt.RunID, AttemptID: attempt.RemoteAttemptID}, Text: m.Text}
			raw, _ := json.Marshal(cmd)
			if _, err = a.s.db.Exec(`UPDATE run_chat SET status='ambiguous',request_json=? WHERE id=?`, string(raw), m.ID); err != nil {
				t.Fatal(err)
			}
			nc := a.workflow.runAPI.conn
			sub, err := nc.Subscribe(reportprotocol.ChatSubject("bonnie", attempt.AgentID), func(msg *nats.Msg) {
				var request reportprotocol.Chat
				if e := json.Unmarshal(msg.Data, &request); e != nil {
					t.Error(e)
					return
				}
				key := a.workflow.runAPI.key.Public().(ed25519.PublicKey)
				if !request.Reconcile || request.ID != cmd.ID || request.Target != cmd.Target || runapitoken.Verify(key, request.Token, attempt.AgentID, time.Now()) != nil {
					t.Error("invalid reconcile command")
				}
				if concurrent {
					if _, e := a.s.db.Exec(`UPDATE run_chat SET status='completed',receipt_json='original' WHERE id=?`, m.ID); e != nil {
						t.Error(e)
						return
					}
				}
				receipt := reportprotocol.ChatReceipt{Version: 1, ID: request.ID, Target: request.Target, Status: "failed", Failure: "reconciled_not_replayed"}
				data, _ := json.Marshal(receipt)
				_ = msg.Respond(data)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			if err = nc.Flush(); err != nil {
				t.Fatal(err)
			}
			err = a.workflow.ReconcileRunChat(t.Context(), record, m.ID)
			if concurrent {
				if !errors.Is(err, ErrRunChatChanged) {
					t.Fatalf("want changed: %v", err)
				}
				// Exercise the same in-flight race through the HTTP boundary too.
				if _, err = a.s.db.Exec(`UPDATE run_chat SET status='ambiguous' WHERE id=?`, m.ID); err != nil {
					t.Fatal(err)
				}
				form := url.Values{"_csrf": {csrf.Value}, "chat_id": {m.ID}}
				status(t, request(a, http.MethodPost, record.basePath()+"/reconcile", form, session, csrf), http.StatusConflict)
			} else if err != nil {
				t.Fatal(err)
			}
			var state, failure, receipt string
			if err = a.s.db.QueryRow(`SELECT status,failure,receipt_json FROM run_chat WHERE id=?`, m.ID).Scan(&state, &failure, &receipt); err != nil {
				t.Fatal(err)
			}
			if concurrent {
				if state != "completed" || receipt != "original" {
					t.Fatal(state, receipt)
				}
			} else {
				var saved reportprotocol.ChatReceipt
				if state != "failed" || failure != "reconciled_not_replayed" || json.Unmarshal([]byte(receipt), &saved) != nil || saved.ID != cmd.ID || saved.Failure != failure {
					t.Fatal(state, failure, receipt)
				}
			}
		})
	}
}

func TestRunDetailReadsWhileWriterReserved(t *testing.T) {
	a := testApp(t)
	attempt := pinnedDetailAttempt(t, a)
	if err := a.workflow.Close(); err != nil {
		t.Fatal(err)
	}
	a.workflow = &Workflow{s: a.s, ctx: t.Context()}
	if _, err := a.s.db.Exec(`UPDATE workflow_attempts SET ready=1 WHERE id=?`, attempt.ID); err != nil {
		t.Fatal(err)
	}
	// A separate connection holds SQLite's write reservation, not the Store's
	// single connection. Readers must still classify chat and load interaction.
	var path string
	var seq int
	var name string
	if err := a.s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	writer := &Store{db: db}
	tx, err := writer.writeTx()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	start := time.Now()
	detail := a.detailModel(r, attempt, runObservation{}, false)
	if ctx.Err() != nil || time.Since(start) >= time.Second || detail.Policy.Mode != "queue" {
		t.Fatal("detail blocked behind writer", ctx.Err())
	}
	var out bytes.Buffer
	if err := runDetailContent(detail).Render(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		t.Fatal("detail not rendered")
	}
}
