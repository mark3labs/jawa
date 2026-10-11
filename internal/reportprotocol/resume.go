package reportprotocol

import (
	"context"
	"encoding/json"
	"errors"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

// FailedResume is JAWA's host control protocol, not a BONNIE Answer. The
// observed journal cursor/turn form a CAS; ID deduplicates ambiguous delivery.
type FailedResume struct {
	Version int           `json:"version"`
	ID      string        `json:"id"`
	Target  client.Target `json:"target"`
	Seq     int           `json:"seq"`
	TurnID  string        `json:"turn_id"`
}
type FailedResumeReply struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Target  client.Target   `json:"target"`
	Status  string          `json:"status"`
	Seq     int             `json:"seq"`
	Outcome *client.Outcome `json:"outcome,omitempty"`
	Error   string          `json:"error,omitempty"`
}

func ResumeSubject(root, agent string) string { return root + ".resume_failed." + agent }
func ResumeFailed(ctx context.Context, nc *nats.Conn, root string, req FailedResume) (FailedResumeReply, error) {
	var reply FailedResumeReply
	data, err := json.Marshal(req)
	if err != nil {
		return reply, err
	}
	msg, err := nc.RequestWithContext(ctx, ResumeSubject(root, req.Target.AgentID), data)
	if err != nil {
		return reply, err
	}
	if len(msg.Data) > 1<<20 {
		return reply, errors.New("resume reply exceeds limit")
	}
	if err = json.Unmarshal(msg.Data, &reply); err != nil {
		return reply, err
	}
	if reply.Version != 1 || reply.ID != req.ID || reply.Target != req.Target {
		return reply, errors.New("invalid resume reply")
	}
	return reply, nil
}
