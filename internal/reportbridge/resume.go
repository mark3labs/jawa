package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"jawa/internal/reportprotocol"
)

const resumeIntentType = "jawa_failed_resume_v1"
const resumeReceiptType = "jawa_failed_resume_receipt_v1"

// Serialize host admission with report commits. A durable intent without a
// receipt is ambiguous after restart: never execute it a second time.
func (b *Bridge) admitResume(ctx context.Context, req reportprotocol.FailedResume) reportprotocol.FailedResumeReply {
	b.submitMu.Lock()
	defer b.submitMu.Unlock()
	// Reconciliation is transport control, not part of the durable command
	// identity: delayed originals and reconciliation retries must deduplicate.
	reconcile := req.Reconcile
	req.Reconcile = false
	reply := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "rejected"}
	if b.resumeStopped || ctx.Err() != nil {
		reply.Error = "agent stopping"
		return reply
	}
	b.mu.RLock()
	r, c := b.runner, b.cfg
	b.mu.RUnlock()
	if req.Version != 1 || req.ID == "" || len(req.ID) > 128 || req.Target.AgentID != c.AgentID || req.Target.TaskID == "" || req.Target.RunID == "" || req.Target.AttemptID == "" || req.TurnID == "" {
		reply.Error = "invalid resume target"
		return reply
	}
	records, err := r.Journal().Replay(ctx, admissionKey(c))
	if err != nil {
		reply.Error = "admission unavailable"
		return reply
	}
	found := false
	for _, rec := range records {
		if rec.ExtType == "nats_task_admitted" {
			var t client.Target
			if json.Unmarshal(rec.Payload, &t) == nil && t == req.Target {
				found = true
			}
		}
	}
	if !found {
		reply.Error = "unknown task ownership"
		return reply
	}
	records, err = r.Journal().Replay(ctx, req.Target.RunID)
	if err != nil {
		reply.Error = "journal unavailable"
		return reply
	}
	var old *reportprotocol.FailedResume
	var receipt *reportprotocol.FailedResumeReply
	terminalIntent := false
	intents := map[string]reportprotocol.FailedResume{}
	for _, rec := range records {
		if rec.ExtType == resumeIntentType {
			var v reportprotocol.FailedResume
			if json.Unmarshal(rec.Payload, &v) == nil {
				v.Reconcile = false
				intents[v.ID] = v
				if v.ID == req.ID {
					old = &v
				}
			}
		}
		if rec.ExtType == resumeReceiptType {
			var v reportprotocol.FailedResumeReply
			if json.Unmarshal(rec.Payload, &v) == nil {
				// A receipt only resolves the intent whose pinned identity it
				// validates, including when handling a different command ID.
				intent, exists := intents[v.ID]
				if exists && v.Version == 1 && v.Target == intent.Target &&
					(v.Status == "ambiguous" || v.Status == "rejected" ||
						(v.Status == "completed" && validCompletedResumeReceipt(intent, v))) {
					delete(intents, v.ID)
				}
				if v.ID == req.ID {
					if old == nil || *old != req || v.Version != 1 || v.Target != req.Target {
						reply.Error = "resume identity conflict"
						return reply
					}
					// Recovery can supersede a legacy receipt. Do not let the
					// first receipt hide a later durable resolution.
					receipt = &v
				}
			}
		}
		if rec.ExtType == intentType {
			var v reportprotocol.Request
			if json.Unmarshal(rec.Payload, &v) == nil && (v.Kind == reportprotocol.CompleteTask || v.Kind == reportprotocol.ReportBlocker) {
				terminalIntent = true
			}
		}
	}
	if receipt != nil {
		if reconcile && receipt.Status == "completed" && !validCompletedResumeReceipt(req, *receipt) {
			reply.Status = "pending"
			if err := b.confirmIdleFailedResume(ctx, r, req.Target.RunID); err != nil {
				reply.Error = err.Error()
				return reply
			}
			return b.finalizeOrphanResume(ctx, r, req)
		}
		return *receipt
	}
	if old != nil {
		if *old != req {
			reply.Error = "resume identity conflict"
			return reply
		}
		if b.resumeInFlight[req] || r.IsActive(req.Target.RunID) {
			reply.Status = "pending"
			return reply
		}
		return b.finalizeOrphanResume(ctx, r, req)
	}
	if reconcile {
		// An unseen command may still be delayed in transport. Only fence it
		// durably after confirming an idle failed snapshot; otherwise pending
		// preserves the possibility that the original is already executing.
		reply.Status = "pending"
		if err := b.confirmIdleFailedResume(ctx, r, req.Target.RunID); err != nil {
			reply.Error = err.Error()
			return reply
		}
		// Do not apply the execution CAS: this command will never execute.
		// The intent must precede its receipt so a failed receipt append cannot
		// allow a delayed original to start the run.
		data, _ := json.Marshal(req)
		if _, err := r.Journal().Append(ctx, runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: data, Timestamp: time.Now().UTC()}); err != nil {
			log.Error("Failed to persist reconciliation intent", "run_id", req.Target.RunID, "resume_id", req.ID)
			reply.Error = "resume intent unavailable"
			return reply
		}
		return b.finalizeOrphanResume(ctx, r, req)
	}
	// Recover idle orphan intents before CAS. Receipt appends advance the cursor,
	// so the caller must refresh rather than execute against its old observation.
	for id, intent := range intents {
		if b.resumeInFlight[intent] || r.IsActive(req.Target.RunID) {
			continue
		}
		if done := b.finalizeOrphanResume(ctx, r, intent); done.Status == "ambiguous" {
			delete(intents, id)
		}
	}
	if len(intents) > 0 {
		reply.Error = "earlier resume remains unconfirmed"
		return reply
	}
	if terminalIntent {
		reply.Error = "terminal report intent prevents resume"
		return reply
	}
	if records, err = r.Journal().Replay(ctx, req.Target.RunID); err != nil {
		reply.Error = "journal unavailable"
		return reply
	}
	run, err := r.Snapshot(ctx, req.Target.RunID)
	if err != nil || run.State != runtime.RunFailed || r.IsActive(req.Target.RunID) || run.TurnID != req.TurnID || len(records) == 0 || records[len(records)-1].Seq != req.Seq {
		reply.Error = "run changed or is not an idle execution failure"
		return reply
	}
	// Preserve the last journalled context; empty input continues the saved
	// conversation without duplicating the original task prompt.
	var lines []string
	for _, rec := range records {
		if rec.Kind == runtime.RecordContext {
			var saved struct {
				Context []string `json:"context"`
			}
			if json.Unmarshal(rec.Payload, &saved) != nil {
				reply.Error = "invalid saved context"
				return reply
			}
			lines = saved.Context
		}
	}
	data, _ := json.Marshal(req)
	if _, err = r.Journal().Append(ctx, runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: data, Timestamp: time.Now().UTC()}); err != nil {
		reply.Error = "resume intent unavailable"
		return reply
	}
	reply.Status = "pending"
	if b.resumeInFlight == nil {
		b.resumeInFlight = make(map[reportprotocol.FailedResume]bool)
	}
	b.resumeInFlight[req] = true
	b.resumeWG.Go(func() {
		defer func() {
			b.submitMu.Lock()
			delete(b.resumeInFlight, req)
			b.submitMu.Unlock()
		}()
		result, e := r.StartRetry(ctx, req.Target.RunID, runtime.Input{Context: lines})
		done := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "completed"}
		if e != nil {
			done.Status = "ambiguous"
			done.Error = "resume execution could not be confirmed"
		} else {
			done.Outcome = &client.Outcome{Version: 1, TaskID: req.Target.TaskID, AgentID: req.Target.AgentID, RunID: req.Target.RunID, AttemptID: req.Target.AttemptID, State: result.State, Response: result.Response, Suspend: result.Suspend}
			if result.Err != nil {
				done.Outcome.Error = result.Err.Error()
			}
			if result.Suspend != nil {
				done.Outcome.AnswerSubject = c.RootSubject + ".answers." + c.AgentID
			}
		}
		// Do not publish completed unless replay supplies a valid outcome cursor.
		if err := saveResumeReceipt(ctx, r, &done); err != nil {
			log.Error("Failed to persist resume receipt", "run_id", req.Target.RunID, "resume_id", req.ID)
		}
	})
	return reply
}

// A completed receipt must describe the command's outcome beyond its CAS.
func validCompletedResumeReceipt(req reportprotocol.FailedResume, receipt reportprotocol.FailedResumeReply) bool {
	out := receipt.Outcome
	return receipt.Seq > req.Seq && out != nil && out.Version == 1 &&
		out.TaskID == req.Target.TaskID && out.AgentID == req.Target.AgentID &&
		out.RunID == req.Target.RunID && out.AttemptID == req.Target.AttemptID
}

// Called under submitMu for reconciliation only. A failed/unavailable snapshot
// cannot safely release a command that might still execute.
func (b *Bridge) confirmIdleFailedResume(ctx context.Context, r *runtime.Runner, runID string) error {
	for intent := range b.resumeInFlight {
		if intent.Target.RunID == runID {
			return errors.New("resume execution remains in flight")
		}
	}
	run, err := r.Snapshot(ctx, runID)
	if err != nil || run.State != runtime.RunFailed || r.IsActive(runID) {
		return errors.New("run is not a confirmed idle execution failure")
	}
	return nil
}

// Called under submitMu. Never infer an outcome or execute an orphan intent.
func (b *Bridge) finalizeOrphanResume(ctx context.Context, r *runtime.Runner, req reportprotocol.FailedResume) reportprotocol.FailedResumeReply {
	done := reportprotocol.FailedResumeReply{Version: 1, ID: req.ID, Target: req.Target, Status: "ambiguous", Error: "resume execution could not be confirmed"}
	if err := saveResumeReceipt(ctx, r, &done); err != nil {
		log.Error("Failed to persist orphan resume receipt", "run_id", req.Target.RunID, "resume_id", req.ID)
		// Only a durable receipt resolves the intent. Allow a later request to
		// retry persistence, not execution.
		done.Status = "pending"
		done.Error = "resume receipt unavailable"
	}
	return done
}

// Receipt recovery must survive request cancellation, but neither replay nor
// append may hold admission/shutdown indefinitely. Each has a bounded context.
func saveResumeReceipt(ctx context.Context, r *runtime.Runner, done *reportprotocol.FailedResumeReply) error {
	replayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	var recs []runtime.Record
	err := retryResumeJournal(replayCtx, "replay", done, func() error {
		var err error
		recs, err = r.Journal().Replay(replayCtx, done.Target.RunID)
		if err == nil && (len(recs) == 0 || recs[len(recs)-1].Seq <= 0) {
			err = errors.New("resume journal has no valid cursor")
		}
		return err
	})
	cancel()
	if err != nil {
		log.Error("Failed to replay resume outcome", "run_id", done.Target.RunID, "resume_id", done.ID)
		done.Status = "ambiguous"
		done.Outcome = nil
		done.Seq = 0
		done.Error = "resume execution could not be confirmed"
	} else {
		done.Seq = recs[len(recs)-1].Seq
	}
	data, err := json.Marshal(done)
	if err != nil {
		return err
	}
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	record := runtime.Record{RunID: done.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeReceiptType, Payload: data, Timestamp: time.Now().UTC()}
	return retryResumeJournal(saveCtx, "append", done, func() error {
		_, err := r.Journal().Append(saveCtx, record)
		return err
	})
}

// Retry only journal operations, never execution. A receipt is immutable, so
// even an append whose acknowledgement was lost is safe to repeat.
func retryResumeJournal(ctx context.Context, operation string, done *reportprotocol.FailedResumeReply, fn func() error) error {
	var err error
	for attempt := range 3 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(); err == nil {
			return nil
		}
		log.Error("Resume journal operation failed", "operation", operation, "run_id", done.Target.RunID, "resume_id", done.ID)
		if attempt < 2 {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return err
}

func (c *reportChannel) startResume(ctx context.Context, nc *nats.Conn) error {
	sub, err := nc.Subscribe(reportprotocol.ResumeSubject(c.cfg.RootSubject, c.cfg.AgentID), func(m *nats.Msg) {
		if !strings.HasPrefix(m.Reply, "_INBOX.") || len(m.Data) > 4096 {
			return
		}
		var req reportprotocol.FailedResume
		if json.Unmarshal(m.Data, &req) != nil {
			return
		}
		reply := c.bridge.admitResume(ctx, req)
		data, e := json.Marshal(reply)
		if e == nil {
			_ = m.Respond(data)
		}
	})
	if err != nil {
		return err
	}
	c.resumeSub = sub
	flushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err = nc.FlushWithContext(flushCtx); err != nil {
		_ = sub.Unsubscribe()
		return errors.New("resume control unavailable")
	}
	return nil
}
