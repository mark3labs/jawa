package orchestrator

import "github.com/charmbracelet/log"

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
