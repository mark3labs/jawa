package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

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
	terminalIntent := false
	intents := map[string]bool{}
	for _, rec := range records {
		if rec.ExtType == resumeIntentType {
			var v reportprotocol.FailedResume
			if json.Unmarshal(rec.Payload, &v) == nil {
				intents[v.ID] = true
				if v.ID == req.ID {
					old = &v
				}
			}
		}
		if rec.ExtType == resumeReceiptType {
			var v reportprotocol.FailedResumeReply
			if json.Unmarshal(rec.Payload, &v) == nil {
				delete(intents, v.ID)
			}
			if v.ID == req.ID {
				if old == nil || *old != req {
					reply.Error = "resume identity conflict"
					return reply
				}
				return v
			}
		}
		if rec.ExtType == intentType {
			var v reportprotocol.Request
			if json.Unmarshal(rec.Payload, &v) == nil && (v.Kind == reportprotocol.CompleteTask || v.Kind == reportprotocol.ReportBlocker) {
				terminalIntent = true
			}
		}
	}
	if old != nil {
		if *old != req {
			reply.Error = "resume identity conflict"
			return reply
		}
		reply.Status = "pending"
		return reply
	}
	if len(intents) > 0 {
		reply.Error = "earlier resume remains unconfirmed"
		return reply
	}
	if terminalIntent {
		reply.Error = "terminal report intent prevents resume"
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
	b.resumeWG.Go(func() {
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
		recs, e := r.Journal().Replay(context.WithoutCancel(ctx), req.Target.RunID)
		if e == nil && len(recs) > 0 {
			done.Seq = recs[len(recs)-1].Seq
		}
		data, _ := json.Marshal(done)
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = r.Journal().Append(saveCtx, runtime.Record{RunID: req.Target.RunID, Kind: runtime.RecordExtensionData, ExtType: resumeReceiptType, Payload: data, Timestamp: time.Now().UTC()})
	})
	return reply
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
