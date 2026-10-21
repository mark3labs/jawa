package orchestrator

import (
	client "github.com/mark3labs/bonnie/client/nats"
	"jawa/internal/orchestrator/db/sqlc"
)

func identityMatches(targetAgent, agentID, runID, remoteID, agent, run, remote string) bool {
	return (targetAgent == "" || targetAgent == agent) && ((agentID == "" && runID == "" && remoteID == "") || (agentID == agent && runID == run && remoteID == remote))
}

func (a Attempt) target() client.Target {
	return client.Target{TaskID: a.TaskID, AgentID: a.AgentID, RunID: a.RunID, AttemptID: a.RemoteAttemptID}
}

// planningAttempt adapts a planning failure for the shared resume checks.
func planningAttempt(t sqlc.PlanningTurn) Attempt {
	return Attempt{ID: t.ID, TaskID: t.ID, State: t.State, RunState: "failed", OutcomeJSON: t.OutcomeJson, AgentID: t.AgentID, RunID: t.RunID, RemoteAttemptID: t.RemoteAttemptID, Error: t.Error, EventSeq: int(t.EventSeq)}
}
