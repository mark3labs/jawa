package orchestrator

type mcpAction struct {
	Tool  string         `json:"tool"`
	Args  map[string]any `json:"args"`
	Needs []string       `json:"needs,omitempty"`
	Note  string         `json:"note,omitempty"`
}
type mcpUnavailable struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

// cardActions mirrors cardMenu and the plan page. Workflow revalidates every action.
func cardActions(d pageData, c Card) ([]mcpAction, []mcpUnavailable) {
	acts, un := []mcpAction{}, []mcpUnavailable{}
	add := func(tool string, args map[string]any, needs ...string) {
		if args == nil {
			args = map[string]any{}
		}
		args["card_id"] = c.ID
		a := mcpAction{Tool: tool, Args: args, Needs: needs}
		if tool == "start_planning" || tool == "start_build" || tool == "approve_plan" {
			a.Note = "optional agent: an id from list_agents"
		}
		acts = append(acts, a)
	}
	latest := latestAttempt(c.ID, d.attempts)
	switch c.Status {
	case "Todo":
		add("start_planning", nil)
		add("start_build", nil)
	case "Planning":
		if detail, ok := d.planning[c.ID]; ok {
			if detail.CanApprove {
				add("approve_plan", map[string]any{"revision": detail.Session.Revision})
			}
			if planningCanReply(detail) {
				add("reply_planning", map[string]any{"expected_interaction": detail.InteractionToken}, "reply")
			}
			if detail.CanResume {
				add("resume_failed", map[string]any{"expected_resume": detail.ResumeToken})
			}
			if detail.Session.State == "failed" {
				add("retry_planning", nil)
			}
		}
		add("cancel_planning", nil)
	}
	if canAction(c, d.attempts, "start") || canAction(c, d.attempts, "retry") {
		add("retry_build", nil)
	}
	if c.Status != "Planning" && canAction(c, d.attempts, "resume") {
		add("resume_failed", map[string]any{"expected_resume": resumeToken(latest[0])})
	}
	if d.canGuide(c) {
		b, _ := d.blocker(c)
		add("guide_blocked", map[string]any{"expected_interaction": b.Token}, "message")
	}
	if c.Status == "Building" || c.Status == "Fix" {
		if canAction(c, d.attempts, "cancel") {
			add("cancel_run", nil)
		} else {
			un = append(un, mcpUnavailable{"cancel_run", "Cancellation requires an active run with a confirmed agent and remote target."})
		}
	}
	if c.Status != "Todo" && c.Status != "Planning" {
		if d.canReset(c) {
			add("reset_card", nil)
		} else {
			un = append(un, mcpUnavailable{"reset_card", d.resetReason(c)})
		}
	}
	add("set_auto_merge", map[string]any{"expected_mode": c.AutoMergeMode}, "mode")
	add("move_card", nil, "position")
	return acts, un
}
