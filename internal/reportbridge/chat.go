package reportbridge

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"time"

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
	if err = appendExt(ctx, b.runner.Journal(), receipt.Target.RunID, chatReceiptType, data); err != nil {
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
	if !validTarget(cmd.Version, cmd.ID, cmd.Target, b.cfg.AgentID) || !validChatText(cmd.Text) {
		reply.Failure = "invalid_command"
		return reply
	}
	r := b.runner
	owned, err := b.ownsTarget(ctx, r, b.cfg, cmd.Target)
	if err != nil {
		reply.Failure = "admission_unavailable"
		return reply
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
	prior, receipt, unresolved, err := chatHistory(records, cmd.ID)
	if err != nil {
		reply.Failure = "journal_invalid"
		return reply
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
	if err = appendExt(ctx, r.Journal(), cmd.Target.RunID, chatIntentType, data); err != nil {
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
	lines, _ := savedContext(records, false)
	if cmd.Followup {
		lines = append(lines, "This is a conversational follow-up to retained historical work. You have no implementation, planning, approval or workflow report authority. Do not resume the old task, change external systems, or call workflow report tools. Answer the new message conversationally. Explicit chat never approves a specification or marks a PR ready.")
	}
	b.executeChat(executionCtx, r, cmd, snap, lines)

	return reply
}

// This route requires both broker access and the same orchestrator-signed
// bearer used by the HTTP-over-NATS API. Never persist its short-lived token.
func (c *reportChannel) startChat(ctx context.Context, nc *nats.Conn) error {
	sub, err := subscribeControl(ctx, nc, reportprotocol.ChatSubject(c.cfg.RootSubject, c.cfg.AgentID), 512*1024, func(ctx context.Context, cmd reportprotocol.Chat) any {
		ioCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		js, err := nc.JetStream(nats.Context(ioCtx))
		if err != nil {
			return nil
		}
		kv, err := js.KeyValue(runapitoken.Bucket)
		if err != nil {
			return nil
		}
		entry, err := kv.Get(runapitoken.PublicKey)
		if err != nil || runapitoken.Verify(ed25519.PublicKey(entry.Value()), cmd.Token, c.cfg.AgentID, time.Now()) != nil {
			return nil
		}
		// Execution lifetime is host-owned, not the bounded admission I/O context.
		return c.bridge.admitChat(ctx, cmd)
	})
	c.chatSub = sub
	if err != nil && sub != nil {
		return errors.New("chat control unavailable")
	}
	return err
}

// executeChat starts only after admission has durably recorded the intent and
// reserved the in-flight slot under submitMu.
func (b *Bridge) executeChat(ctx context.Context, r *runtime.Runner, cmd reportprotocol.Chat, snap *runtime.Run, lines []string) {
	b.resumeWG.Go(func() {
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
			done.Outcome = runOutcome(cmd.Target, b.cfg, run.State, run.Response, run.Suspend)
			if run.Err != nil {
				done.Outcome.Error = "chat execution failed"
			}
		}
		b.submitMu.Lock()
		b.appendChatReceipt(ctx, done)
		b.submitMu.Unlock()
	})
}

// chatHistory retains the last matching receipt and tracks unresolved intents
// in journal order. Malformed intents fail closed; malformed receipts do not
// resolve a command.
func chatHistory(records []runtime.Record, id string) (*reportprotocol.Chat, *reportprotocol.ChatReceipt, map[string]bool, error) {
	var prior *reportprotocol.Chat
	var receipt *reportprotocol.ChatReceipt
	unresolved := map[string]bool{}
	for _, rec := range records {
		if rec.ExtType == chatIntentType {
			var old reportprotocol.Chat
			if err := json.Unmarshal(rec.Payload, &old); err != nil {
				return nil, nil, nil, err
			}
			unresolved[old.ID] = true
			if old.ID == id {
				prior = &old
			}
		}
		if rec.ExtType == chatReceiptType {
			var saved reportprotocol.ChatReceipt
			if json.Unmarshal(rec.Payload, &saved) == nil {
				if saved.Status == "completed" || saved.Status == "failed" {
					delete(unresolved, saved.ID)
				}
				if saved.ID == id {
					receipt = &saved
				}
			}
		}
	}
	return prior, receipt, unresolved, nil
}

// Called under submitMu so in-flight admission and receipt persistence cannot
// race the resume authority check.
func (b *Bridge) hasActiveChat(records []runtime.Record) bool {
	for _, rec := range records {
		if rec.ExtType == chatIntentType {
			var chat reportprotocol.Chat
			if json.Unmarshal(rec.Payload, &chat) == nil && b.chatInFlight[chat.ID] {
				return true
			}
		}
	}
	return false
}
