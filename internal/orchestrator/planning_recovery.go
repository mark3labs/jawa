package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/nats-io/nats.go"
)

// Read retained outcomes without advancing consumers, including outcomes that were
// acknowledged before a restore. Queue entries are independently resumed by dispatch.
func (w *Workflow) recoverPlanningResults(conn *nats.Conn) error {
	ctx, cancel := context.WithTimeout(w.ctx, 5*time.Second)
	defer cancel()
	rows, err := w.s.db.QueryContext(ctx, `SELECT t.id FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id JOIN cards c ON c.id=s.card_id WHERE c.status='Planning' AND s.state IN ('queued','running','failed') AND t.outcome_json='' AND t.state IN ('queued','submitted','running','failed') AND t.number=(SELECT max(number) FROM planning_turns WHERE session_id=s.id)`)
	if err != nil {
		return err
	}
	tasks := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		tasks[id] = true
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil || len(tasks) == 0 {
		return err
	}
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	stream := client.DefaultResultStreamName("bonnie.results")
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
		if err = ctx.Err(); err != nil {
			return err
		}
		msg, e := js.GetMsg(stream, seq, nats.Context(ctx))
		if errors.Is(e, nats.ErrMsgNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		var out client.Outcome
		if msg.Subject == "bonnie.results" && json.Unmarshal(msg.Data, &out) == nil && tasks[out.TaskID] {
			if err = w.result(ctx, out); err != nil {
				return err
			}
		}
		if seq == last {
			break
		}
	}
	return nil
}
