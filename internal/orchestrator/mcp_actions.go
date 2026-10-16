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
	if c.Archived {
		add("restore_card", nil)
	} else {
		add("archive_card", nil)
	}
	if c.Status == "Done" {
		if canAction(c, d.attempts, "fix") {
			a, check := latest[0], d.prChecks[c.ID]
			if check.State != "merged" {
				add("mark_pr_merged", map[string]any{"expected_attempt_id": a.ID, "expected_pr_url": a.PRURL})
			}
			if check.State == "conflict" {
				add("fix_card", map[string]any{"message": conflictFixMessage})
				acts[len(acts)-1].Note = "prefilled conflict repair message; edit freely"
			} else {
				add("fix_card", nil, "message")
			}
			add("refresh_pr_checks", map[string]any{"project_id": c.ProjectID})
			if d.canMerge(c) {
				p, _ := selectedProject(d.projects, c.ProjectID)
				add("merge_pr", map[string]any{"snapshot": mergeSnapshot(c, a, p, check.HeadSHA, check.BaseSHA), "method": "merge"}, "idempotency_key")
				acts[len(acts)-1].Note = "method may be merge, squash or rebase; reuse the same idempotency_key to retry"
			}
		} else {
			un = append(un, mcpUnavailable{"fix_card", "Fix requires a verified pull request and no active attempt."})
		}
	}
	if d.canForceReady(c) {
		a := latest[0]
		p, _ := selectedProject(d.projects, c.ProjectID)
		pr, _ := forceReadyPR(c, a, p, "")
		add("force_ready", map[string]any{"expected": forceReadyToken(c, a, p)}, "pr_number", "acknowledge")
		acts[len(acts)-1].Note = "Bypasses CI and review checks; does not merge. set acknowledge: true only after a human-equivalent decision"
		if pr != "" {
			acts[len(acts)-1].Note += " Known PR: " + pr
		}
	}
	if canAction(c, d.attempts, "force-stop") {
		add("force_stop", nil, "acknowledge")
		acts[len(acts)-1].Note = "set acknowledge: true only after a human-equivalent decision"
	}
	if canAction(c, d.attempts, "delete") {
		args := map[string]any{}
		if cardNeedsReconciliation(c, d.attempts) {
			args["acknowledge_orphan"] = true
		}
		add("delete_card", args, "acknowledge")
		acts[len(acts)-1].Note = "set acknowledge: true only after a human-equivalent decision"
	} else {
		un = append(un, mcpUnavailable{"delete_card", actionReason(c, d.attempts, "delete")})
	}
	add("set_auto_merge", map[string]any{"expected_mode": c.AutoMergeMode}, "mode")
	add("move_card", nil, "position")
	return acts, un
}
