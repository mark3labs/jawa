package orchestrator

// Authoritative card-action guards shared by workflow merge eligibility, MCP and UI.
func canAction(c Card, attempts []Attempt, action string) bool {
	active := cardHasActiveAttempt(c.ID, attempts)
	latest := latestAttempt(c.ID, attempts)
	switch action {
	case "force-ready":
		return canForceReady(c, attempts)
	case "start":
		return canStart(c, latest, active)
	case "resume":
		return canResume(c, latest, active)
	case "retry":
		return canRetry(c, latest, active)
	case "fix":
		return canFix(c, latest, active)
	case "reset":
		return canReset(c, attempts)
	case "delete":
		return canDelete(latest, active)
	case "cancel":
		return canCancel(c, latest)
	case "force-stop":
		return canForceStop(c, attempts, latest)
	}
	return false
}

func canForceReady(c Card, attempts []Attempt) bool {
	return forceReadyReason(c, attempts, false) == ""
}

func canStart(c Card, latest []Attempt, active bool) bool {
	return c.Status == "Building" && len(latest) == 0 && !active
}

func canResume(c Card, latest []Attempt, active bool) bool {
	return (c.Status == "Building" || c.Status == "Fix") && len(latest) > 0 && !active && executionFailure(latest[0])
}

func canRetry(c Card, latest []Attempt, active bool) bool {
	return (c.Status == "Building" || c.Status == "Fix") && len(latest) > 0 && terminalFailure(latest[0]) && !active
}

func canFix(c Card, latest []Attempt, active bool) bool {
	return c.Status == "Done" && len(latest) > 0 && latest[0].Ready && latest[0].State == "ready" && latest[0].PRURL != "" && !active
}

func canReset(c Card, attempts []Attempt) bool {
	return resetReason(c, attempts, false) == ""
}

func canDelete(latest []Attempt, active bool) bool {
	return !active || (len(latest) > 0 && !latest[0].Ready && latest[0].Error == executionConflictNotice)
}

func canCancel(c Card, latest []Attempt) bool {
	if (c.Status != "Building" && c.Status != "Fix") || len(latest) == 0 {
		return false
	}
	a := latest[0]
	return a.AgentID != "" && a.RunID != "" && a.RemoteAttemptID != "" && (a.State == "running" || a.State == "waiting" || a.State == "interrupted" || a.RunState == "waiting")
}

func canForceStop(c Card, attempts, latest []Attempt) bool {
	if c.Status == "Planning" || len(latest) == 0 {
		return false
	}
	for _, a := range attempts {
		if a.CardID == c.ID && forceStoppable(a, a.ID == latest[0].ID) {
			return true
		}
	}
	return false
}

func cardNeedsReconciliation(c Card, attempts []Attempt) bool {
	latest := latestAttempt(c.ID, attempts)
	return len(latest) > 0 && !latest[0].Ready && latest[0].Error == executionConflictNotice
}

func actionReason(c Card, attempts []Attempt, action string) string {
	if action == "force-ready" {
		return forceReadyReason(c, attempts, false)
	}
	if action == "reset" {
		return resetReason(c, attempts, false)
	}
	if cardHasActiveAttempt(c.ID, attempts) {
		return "Active attempt — wait for it to finish, or use Force stop."
	}
	if latest := latestAttempt(c.ID, attempts); len(latest) > 0 && forceStoppable(latest[0], true) {
		return "Attempt has not finished — use Force stop to release it."
	}
	return "Reset requires a failed or blocked attempt."
}
