package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

func assignmentSignals(mode string, needsAck bool) string {
	data, _ := json.Marshal(map[string]any{"assignMode": mode, "assignMessage": "", "assignAck": false, "assignNeedsAck": needsAck})
	return string(data)
}
func assignLabel(mode string, needsAck bool) string {
	if needsAck {
		return "Approve & build with agent"
	}
	switch mode {
	case "plan":
		return "Plan with agent"
	case "fix":
		return "Fix with agent"
	}
	return "Build with agent"
}

// assignmentRouteMode maps a dialog mode to its agent role; Fix is building.
func assignmentRouteMode(mode string) string {
	if mode == "plan" {
		return routePlan
	}
	return routeBuild
}
func agentOptions(mode string, choices []AgentChoice) []selectOption {
	options := []selectOption{{Value: "", Label: "Select an agent"}}
	route := assignmentRouteMode(mode)
	for _, c := range choices {
		label := fmt.Sprintf("%s · %s / %s · %s", c.ID, c.Provider, c.Model, c.Availability)
		allowed := c.allows(route)
		if !c.Selectable {
			label += " · Cannot accept tasks"
		} else if !allowed {
			label += " · Disabled for " + routeModeLabel(route)
		}
		options = append(options, selectOption{Value: c.ID, Label: label, Disabled: !c.Selectable || !allowed})
	}
	return options
}
func agentChoiceStatus(choices []AgentChoice, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Agent loading timed out. Refresh to retry."
	}
	if err != nil {
		return "Unable to load agents. Refresh to retry."
	}
	if len(choices) == 0 {
		return "No connected agents. Refresh to check again."
	}
	return "Busy agents can be selected; work may wait."
}
func (a *app) assignmentAgents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	choices, err := a.workflow.choices(ctx)
	mode := r.URL.Query().Get("mode")
	if mode != routePlan {
		mode = routeBuild
	}
	var buf bytes.Buffer
	if assignmentAgentField(mode, choices, err).Render(ctx, &buf) != nil {
		return
	}
	_ = newSSEWriter(w).write(elementsFrameMode("replace", "", buf.String()))
}
func issueOptions(issues []projectIssue) []selectOption {
	options := []selectOption{{Value: "", Label: "No linked issue"}}
	for _, i := range issues {
		options = append(options, selectOption{Value: i.URL, Label: fmt.Sprintf("#%d %s", i.Number, i.Title)})
	}
	return options
}
func issueRecoveryBlocked(state string) bool {
	return state == "reserved" || state == "uncertain" || state == "rejected"
}
func (a *app) issuesFor(ctx context.Context, projectID string) ([]projectIssue, error) {
	projects, err := a.s.Projects()
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.ID == projectID {
			settings, err := a.s.providerSettings()
			if err != nil {
				return nil, err
			}
			return listProjectIssues(ctx, p, a.issueClient, settings)
		}
	}
	return nil, errors.New("unknown project")
}
func (a *app) cardIssues(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var issues []projectIssue
	status := "Choose a project to load issues. You can submit without linking an issue."
	if projectID := r.URL.Query().Get("project_id"); projectID != "" {
		var err error
		issues, err = a.issuesFor(ctx, projectID)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = "Loading issues timed out. Refresh issues to retry, or submit without linking an issue."
		case err != nil:
			status = "Unable to load issues. Refresh issues to retry, or submit without linking an issue."
		case len(issues) == 0:
			status = "No open issues found. You can submit without linking an issue. Refresh issues to check again."
		default:
			status = "Choose an issue, or leave No linked issue selected."
		}
	}
	var field, token bytes.Buffer
	if cardIssueField(issues, status, "").Render(ctx, &field) != nil || issueSubmissionToken(newID()).Render(ctx, &token) != nil {
		return
	}
	if issues == nil {
		issues = []projectIssue{}
	}
	signals, _ := json.Marshal(map[string]any{"_issues": issues})
	stream := newSSEWriter(w)
	if stream.write(elementsFrameMode("replace", "", field.String())) != nil {
		return
	}
	if stream.write(elementsFrame(token.String())) != nil {
		return
	}
	_ = stream.write("event: datastar-patch-signals\ndata: signals " + string(signals) + "\n\n")
}
