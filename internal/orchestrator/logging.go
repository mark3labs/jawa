package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/nats-io/nats.go"
)

// Classify errors using fixed labels only; never expose payloads or endpoints.
func consumerErrorCategory(err error) string {
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax) || errors.As(err, &value):
		return "malformed-message"
	case errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrDisconnected):
		return "broker-disconnected"
	case errors.Is(err, nats.ErrConsumerNotFound):
		return "consumer-not-found"
	case errors.Is(err, nats.ErrStreamNotFound):
		return "stream-not-found"
	}
	for _, stage := range []string{"invalid status event", "invalid result version", "message exceeds limit", "bind results", "fetch results", "acknowledge result", "heartbeat", "retry result", "unsubscribe"} {
		if strings.Contains(err.Error(), "bonnie: client/nats: "+stage) {
			return strings.ReplaceAll(stage, " ", "-")
		}
	}
	return "handler-or-storage"
}

func (w *Workflow) log() *log.Logger {
	if w.logger != nil {
		return w.logger
	}
	return log.Default()
}

// Deliberately exclude prompts, result payloads, URLs and remote error text.
func (w *Workflow) logAttempt(level log.Level, message string, a Attempt) {
	w.log().Log(level, message, "card_id", a.CardID, "task_id", a.TaskID,
		"attempt_id", a.ID, "agent_id", a.AgentID, "run_id", a.RunID,
		"remote_attempt_id", a.RemoteAttemptID, "state", a.State, "run_state", a.RunState)
}
