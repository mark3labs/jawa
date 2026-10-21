package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"jawa/internal/reportprotocol"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

const admittedType = "nats_task_admitted"

func validTarget(version int, id string, t client.Target, agentID string) bool {
	return version == 1 && id != "" && len(id) <= 128 && t.AgentID == agentID && t.TaskID != "" && t.RunID != "" && t.AttemptID != ""
}

func (b *Bridge) ownsTarget(ctx context.Context, r *runtime.Runner, cfg natschannel.Config, t client.Target) (bool, error) {
	records, err := r.Journal().Replay(ctx, admissionKey(cfg))
	if err != nil {
		return false, err
	}
	for _, rec := range records {
		var target client.Target
		if rec.ExtType == admittedType && json.Unmarshal(rec.Payload, &target) == nil && target == t {
			return true, nil
		}
	}
	return false, nil
}

// Strict resume recovery rejects any malformed context; chat retains the last
// valid context and ignores malformed records.
func savedContext(records []runtime.Record, strict bool) ([]string, error) {
	var lines []string
	for _, rec := range records {
		if rec.Kind != runtime.RecordContext {
			continue
		}
		var saved struct {
			Context []string `json:"context"`
		}
		if err := json.Unmarshal(rec.Payload, &saved); err != nil {
			if strict {
				return nil, err
			}
			continue
		}
		lines = saved.Context
	}
	return lines, nil
}

// Callers retain their command-specific execution error policy.
func runOutcome(t client.Target, cfg natschannel.Config, state runtime.RunState, resp string, suspend *runtime.SuspendRequest) *client.Outcome {
	out := &client.Outcome{Version: 1, TaskID: t.TaskID, AgentID: t.AgentID, RunID: t.RunID, AttemptID: t.AttemptID, State: state, Response: resp, Suspend: suspend}
	if suspend != nil {
		out.AnswerSubject = cfg.RootSubject + ".answers." + cfg.AgentID
	}
	return out
}

func appendExt(ctx context.Context, j runtime.Journal, runID, extType string, payload []byte) error {
	_, err := j.Append(ctx, runtime.Record{RunID: runID, Kind: runtime.RecordExtensionData, ExtType: extType, Payload: payload, Timestamp: time.Now().UTC()})
	return err
}

// A nil response suppresses replies (for example when chat authentication
// fails). The handler receives the host context, not the subscription flush's
// bounded I/O context, so admitted execution remains host-owned.
func subscribeControl[T any](ctx context.Context, nc *nats.Conn, subject string, maxBytes int, handle func(context.Context, T) any) (*nats.Subscription, error) {
	sub, err := nc.Subscribe(subject, func(m *nats.Msg) {
		if !strings.HasPrefix(m.Reply, "_INBOX.") || len(m.Data) > maxBytes {
			return
		}
		var req T
		if json.Unmarshal(m.Data, &req) != nil {
			return
		}
		reply := handle(ctx, req)
		if reply == nil {
			return
		}
		if data, err := json.Marshal(reply); err == nil {
			_ = m.Respond(data)
		}
	})
	if err != nil {
		return nil, err
	}
	flush, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := nc.FlushWithContext(flush); err != nil {
		_ = sub.Unsubscribe()
		return sub, errors.New("control unavailable")
	}
	return sub, nil
}

func validChatText(text string) bool {
	return strings.TrimSpace(text) != "" && len(text) <= 64000
}

func matchesResumeObservation(r *runtime.Runner, req reportprotocol.FailedResume, run *runtime.Run, records []runtime.Record) bool {
	return run.State == runtime.RunFailed && !r.IsActive(req.Target.RunID) && run.TurnID == req.TurnID && len(records) > 0 && records[len(records)-1].Seq == req.Seq
}
