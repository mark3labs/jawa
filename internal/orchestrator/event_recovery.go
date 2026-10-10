package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	protocol "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

// RecoverEvents reads retained status history without advancing any consumer.
// This can discover the exact execution when its acceptance was missed.
func (w *Workflow) RecoverEvents(ctx context.Context, conn *nats.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	stream := protocol.DefaultEventStreamName("bonnie.events")
	info, err := js.StreamInfo(stream, nats.Context(ctx))
	if errors.Is(err, nats.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.State.Msgs == 0 {
		return nil
	}
	first, last := info.State.FirstSeq, info.State.LastSeq
	if last-first >= 10000 {
		first = last - 9999
	}
	for seq := first; seq <= last; seq++ {
		msg, err := js.GetMsg(stream, seq, nats.Context(ctx))
		if errors.Is(err, nats.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if msg.Subject != "bonnie.events" {
			continue
		}
		var ev client.StatusEvent
		if json.Unmarshal(msg.Data, &ev) != nil || ev.Version != 1 || ev.EventID == "" || ev.AgentID == "" || ev.RunID == "" || ev.AttemptID == "" || (ev.Type != "run_state" && ev.Type != "task_accepted") {
			continue
		}
		if err := w.event(ctx, ev); err != nil {
			return err
		}
		if seq == last {
			break
		}
	}
	return nil
}

// Independent boards must not compete for one durable subscription.
func workflowConsumerID(s *Store) (string, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES('workflow_consumer_id',?)`, newID()); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key='workflow_consumer_id'`).Scan(&id)
	return id, err
}
