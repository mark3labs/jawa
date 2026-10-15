package reportbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
	"jawa/internal/runapitoken"
)

const chatIntentType = "jawa_run_chat_v1"
const chatReceiptType = "jawa_run_chat_receipt_v1"

// chatFollowup fences report authority from intent onward, including the gap
// before a new turn record and recovery after process death.
func chatFollowup(records []runtime.Record) bool {
	followup := false
	for _, rec := range records {
		if rec.ExtType == chatIntentType {
			var cmd reportprotocol.Chat
			if json.Unmarshal(rec.Payload, &cmd) == nil {
				followup = followup || cmd.Followup
			}
		}
	}
	return followup
}

func (b *Bridge) appendChatReceipt(ctx context.Context, receipt reportprotocol.ChatReceipt) reportprotocol.ChatReceipt {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	records, err := b.runner.Journal().Replay(ctx, receipt.Target.RunID)
	if err == nil && len(records) > 0 {
		receipt.Seq = records[len(records)-1].Seq
	}
	if session, e := runtime.Restore(ctx, receipt.Target.RunID, b.runner.Journal()); e == nil {
		// The final receipt binds the command to its durable user message index.
		// Match only this admitted turn's text; unrelated equal earlier messages
		// are excluded by the intent cursor.
		var text string
		var watermark int
		for _, record := range records {
			if record.ExtType == chatIntentType {
				var cmd reportprotocol.Chat
				if json.Unmarshal(record.Payload, &cmd) == nil && cmd.ID == receipt.ID {
					text = cmd.Text
					watermark = record.Seq
				}
			}
		}
		hasInput := false
		for _, record := range records {
			if record.Seq > watermark && record.Kind == runtime.RecordMessage && record.Role == "user" && record.Text == text {
				hasInput = true
			}
		}
		if hasInput {
			for index, message := range session.GetMessages() {
				if message.Role == "user" {
					for _, part := range message.Content {
						if p, ok := part.(kit.LLMTextPart); ok && p.Text == text {
							i := index
							receipt.UserIndex = &i
						}
					}
				}
			}
		}
	}
	data, _ := json.Marshal(receipt)
	if _, err = b.runner.Journal().Append(ctx, runtime.Record{RunID: receipt.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: chatReceiptType, Payload: data, Timestamp: time.Now().UTC()}); err != nil {
		receipt.Status, receipt.Failure = "active", "receipt_unavailable"
	}
	return receipt
}

// All admissions share the report/resume mutex. Intent is durable before model
// execution; an orphan is never automatically executed a second time.
func (b *Bridge) admitChat(ctx context.Context, cmd reportprotocol.Chat) reportprotocol.ChatReceipt {
	executionCtx := ctx
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b.submitMu.Lock()
	defer b.submitMu.Unlock()
	reply := reportprotocol.ChatReceipt{Version: 1, ID: cmd.ID, Target: cmd.Target, Status: "failed"}
	if b.resumeStopped || ctx.Err() != nil {
		reply.Failure = "stopping"
		return reply
	}
	reconcile := cmd.Reconcile
	cmd.Token, cmd.Reconcile = "", false
	if cmd.Version != 1 || cmd.ID == "" || len(cmd.ID) > 128 || cmd.Target.AgentID != b.cfg.AgentID || cmd.Target.TaskID == "" || cmd.Target.RunID == "" || cmd.Target.AttemptID == "" || strings.TrimSpace(cmd.Text) == "" || len(cmd.Text) > 64000 {
		reply.Failure = "invalid_command"
		return reply
	}
	r := b.runner
	admissions, err := r.Journal().Replay(ctx, admissionKey(b.cfg))
	if err != nil {
		reply.Failure = "admission_unavailable"
		return reply
	}
	owned := false
	for _, rec := range admissions {
		var target client.Target
		if rec.ExtType == "nats_task_admitted" && json.Unmarshal(rec.Payload, &target) == nil && target == cmd.Target {
			owned = true
		}
	}
	if !owned {
		reply.Failure = "unknown_owner"
		return reply
	}
	records, err := r.Journal().Replay(ctx, cmd.Target.RunID)
	if err != nil || len(records) == 0 {
		reply.Failure = "journal_unavailable"
		return reply
	}
	var prior *reportprotocol.Chat
	var receipt *reportprotocol.ChatReceipt
	unresolved := map[string]bool{}
	for _, rec := range records {
		if rec.ExtType == chatIntentType {
			var old reportprotocol.Chat
			if json.Unmarshal(rec.Payload, &old) != nil {
				reply.Failure = "journal_invalid"
				return reply
			}
			unresolved[old.ID] = true
			if old.ID == cmd.ID {
				prior = &old
			}
		}
		if rec.ExtType == chatReceiptType {
			var saved reportprotocol.ChatReceipt
			if json.Unmarshal(rec.Payload, &saved) == nil {
				if saved.Status == "completed" || saved.Status == "failed" {
					delete(unresolved, saved.ID)
				}
				if saved.ID == cmd.ID {
					receipt = &saved
				}
			}
		}
	}
	if prior != nil {
		if *prior != cmd {
			reply.Status, reply.Failure = "ambiguous", "identity_conflict"
			return reply
		}
		if receipt != nil {
			if reconcile && receipt.Status == "ambiguous" && !b.chatInFlight[cmd.ID] && !r.IsActive(cmd.Target.RunID) {
				receipt.Status, receipt.Failure = "failed", "reconciled_not_replayed"
				return b.appendChatReceipt(ctx, *receipt)
			}
			return *receipt
		}
		if b.chatInFlight[cmd.ID] || r.IsActive(cmd.Target.RunID) {
			reply.Status = "active"
			return reply
		}
		reply.Status, reply.Failure = "ambiguous", "execution_unconfirmed"
		return b.appendChatReceipt(ctx, reply)
	}
	if len(unresolved) > 0 {
		reply.Status, reply.Failure = "queued", "prior_command_unconfirmed"
		return reply
	}
	snap, err := r.Snapshot(ctx, cmd.Target.RunID)
	if err != nil {
		reply.Failure = "journal_unavailable"
		return reply
	}
	if chatFollowup(records) && !cmd.Followup {
		reply.Failure = "authority_revoked"
		return reply
	}
	active := r.IsActive(cmd.Target.RunID)
	if active && (cmd.Followup || reconcile) {
		reply.Status, reply.Failure = "queued", "turn_active"
		return reply
	}
	if snap.State == runtime.RunRetired {
		reply.Failure = "retired"
		return reply
	}
	if !reconcile && (snap.TurnID != cmd.TurnID || records[len(records)-1].Seq != cmd.Seq) {
		reply.Status, reply.Failure = "queued", "observation_changed"
		return reply
	}
	if !reconcile && snap.State == runtime.RunWaiting && !cmd.Followup {
		reply.Status, reply.Failure = "queued", "interaction_pending"
		return reply
	}
	// A follow-up may not take over an active authoritative generation. The
	// orchestrator sends it only after normal work is idle or locally fenced.
	data, _ := json.Marshal(cmd)
	if _, err = r.Journal().Append(ctx, runtime.Record{RunID: cmd.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: chatIntentType, Payload: data, Timestamp: time.Now().UTC()}); err != nil {
		reply.Failure = "intent_unavailable"
		return reply
	}
	if active && !reconcile {
		if err := r.Steer(cmd.Target.RunID, cmd.Text); err != nil {
			reply.Status, reply.Failure = "ambiguous", "steering_unconfirmed"
		} else {
			reply.Status = "completed"
			reply.TurnID = snap.TurnID
		}
		return b.appendChatReceipt(ctx, reply)
	}
	if reconcile {
		reply.Status, reply.Failure = "failed", "reconciled_not_replayed"
		return b.appendChatReceipt(ctx, reply)
	}
	if b.chatInFlight == nil {
		b.chatInFlight = map[string]bool{}
	}
	b.chatInFlight[cmd.ID] = true
	reply.Status = "active"
	var lines []string
	for _, rec := range records {
		if rec.Kind == runtime.RecordContext {
			var saved struct {
				Context []string `json:"context"`
			}
			if json.Unmarshal(rec.Payload, &saved) == nil {
				lines = saved.Context
			}
		}
	}
	if cmd.Followup {
		lines = append(lines, "This is a conversational follow-up to retained historical work. You have no implementation, planning, approval or workflow report authority. Do not resume the old task, change external systems, or call workflow report tools. Answer the new message conversationally. Explicit chat never approves a specification or marks a PR ready.")
	}
	b.resumeWG.Go(func() {
		ctx := executionCtx
		defer func() { b.submitMu.Lock(); delete(b.chatInFlight, cmd.ID); b.submitMu.Unlock() }()
		done := reportprotocol.ChatReceipt{Version: 1, ID: cmd.ID, Target: cmd.Target, Status: "completed"}
		var run *runtime.Run
		var callErr error
		// Explicit input ends an idle interrupted logical turn. Exact-turn
		// cancellation closes pending completion orchestration; it does not
		// erase the conversation, sandbox binding, or workflow failure evidence.
		// StartRetry would instead execute old work before the new message.
		if snap.State == runtime.RunInterrupted {
			_, callErr = r.RequestCancel(ctx, cmd.Target.RunID, snap.TurnID)
		}
		if callErr == nil && snap.State == runtime.RunWaiting && cmd.Followup {
			response := runtime.InputResponse{TurnID: snap.TurnID, Text: cmd.Text}
			if snap.Suspend != nil && snap.Suspend.Kind == runtime.SuspendApproval {
				no := false
				response.Approved = &no
			}
			run, callErr = r.Resume(ctx, cmd.Target.RunID, []runtime.InputResponse{response})
		} else if callErr == nil {
			// Saved recovery may have parked at a new interaction: the explicit text
			// remains undelivered, never guessed into that interaction.
			run, callErr = r.Start(ctx, cmd.Target.RunID, runtime.Input{Text: cmd.Text, Context: lines})
		}
		if errors.Is(callErr, runtime.ErrInputDeferred) || errors.Is(callErr, runtime.ErrRunWaiting) {
			done.Status, done.Failure = "failed", "input_deferred"
		} else if run == nil {
			done.Status, done.Failure = "ambiguous", "execution_unconfirmed"
		} else {
			done.TurnID = run.TurnID
			done.Outcome = &client.Outcome{Version: 1, TaskID: cmd.Target.TaskID, AgentID: cmd.Target.AgentID, RunID: cmd.Target.RunID, AttemptID: cmd.Target.AttemptID, State: run.State, Response: run.Response, Suspend: run.Suspend}
			if run.Err != nil {
				done.Outcome.Error = "chat execution failed"
			}
			if run.Suspend != nil {
				done.Outcome.AnswerSubject = b.cfg.RootSubject + ".answers." + b.cfg.AgentID
			}
		}
		b.submitMu.Lock()
		b.appendChatReceipt(ctx, done)
		b.submitMu.Unlock()
	})
	return reply
}

// This route requires both broker access and the same orchestrator-signed
// bearer used by the HTTP-over-NATS API. Never persist its short-lived token.
func (c *reportChannel) startChat(ctx context.Context, nc *nats.Conn) error {
	sub, err := nc.Subscribe(reportprotocol.ChatSubject(c.cfg.RootSubject, c.cfg.AgentID), func(m *nats.Msg) {
		if !strings.HasPrefix(m.Reply, "_INBOX.") || len(m.Data) > 80000 {
			return
		}
		var cmd reportprotocol.Chat
		if json.Unmarshal(m.Data, &cmd) != nil {
			return
		}
		ioCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		js, err := nc.JetStream(nats.Context(ioCtx))
		if err != nil {
			return
		}
		kv, err := js.KeyValue(runapitoken.Bucket)
		if err != nil {
			return
		}
		entry, err := kv.Get(runapitoken.PublicKey)
		if err != nil || runapitoken.Verify(ed25519.PublicKey(entry.Value()), cmd.Token, c.cfg.AgentID, time.Now()) != nil {
			return
		}
		// Execution lifetime is host-owned, not the bounded admission I/O context.
		reply := c.bridge.admitChat(ctx, cmd)
		data, _ := json.Marshal(reply)
		_ = m.Respond(data)
	})
	if err != nil {
		return err
	}
	c.chatSub = sub
	flush, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = nc.FlushWithContext(flush); err != nil {
		_ = sub.Unsubscribe()
		return errors.New("chat control unavailable")
	}
	return nil
}
