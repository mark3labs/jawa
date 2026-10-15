package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/presence"
)

// AgentChoice intentionally excludes transport addresses and arbitrary labels.
type AgentChoice struct {
	ID           string `json:"id"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Availability string `json:"availability"`
	Selectable   bool   `json:"selectable"`
}

func validTargetID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func agentChoices(records []presence.Record) []AgentChoice {
	// Sort first so duplicate IDs have deterministic metadata.
	sort.Slice(records, func(i, j int) bool {
		if records[i].Identity.Agent == records[j].Identity.Agent {
			return records[i].Identity.Instance < records[j].Identity.Instance
		}
		return records[i].Identity.Agent < records[j].Identity.Agent
	})
	choices := []AgentChoice{}
	for _, record := range records {
		id := record.Identity.Agent
		if len(choices) > 0 && choices[len(choices)-1].ID == id {
			continue
		}
		selectable := false
		for _, endpoint := range record.Endpoints {
			if endpoint.Input && endpoint.Channel == "nats" && (endpoint.Address == "bonnie.tasks" || endpoint.Address == "bonnie.tasks.agent."+id) {
				selectable = validTargetID(id)
			}
		}
		availability := "Busy / not ready"
		if agentReady(record) {
			availability = "Available"
		}
		choices = append(choices, AgentChoice{id, agentModelLabel(record, "jawa.provider"), agentModelLabel(record, "jawa.model"), availability, selectable})
	}
	return choices
}

func (w *Workflow) choices(ctx context.Context) ([]AgentChoice, error) {
	if w == nil || w.presence == nil {
		return nil, errors.New("agent discovery unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	records, err := w.Agents(ctx)
	if err != nil {
		return nil, fmt.Errorf("agent discovery unavailable: %w", err)
	}
	return agentChoices(records), nil
}

func (w *Workflow) validateTarget(ctx context.Context, target string) error {
	if !validTargetID(target) {
		return errors.New("select a valid agent")
	}
	choices, err := w.choices(ctx)
	if err != nil {
		return err
	}
	for _, choice := range choices {
		if choice.ID == target && choice.Selectable {
			return nil
		}
	}
	return errors.New("selected agent is disconnected or cannot accept tasks; refresh agents")
}

func (w *Workflow) PlanWithAgent(ctx context.Context, id, target string) error {
	if err := w.validateTarget(ctx, target); err != nil {
		return err
	}
	return w.startPlanningTo(id, 0, target)
}
func (w *Workflow) BuildWithAgent(ctx context.Context, id, target string) error {
	if err := w.validateTarget(ctx, target); err != nil {
		return err
	}
	return w.moveCardTo(id, "Building", 0, false, target)
}
func (w *Workflow) ApprovePlanningWithAgent(ctx context.Context, id string, revision int, target string) error {
	if err := w.validateTarget(ctx, target); err != nil {
		return err
	}
	return w.approvePlanningTo(id, revision, target)
}
func (w *Workflow) FixWithAgent(ctx context.Context, id, message, target string) error {
	if err := w.validateTarget(ctx, target); err != nil {
		return err
	}
	return w.fixCardTo(id, message, target)
}

func (w *Workflow) submitTask(ctx context.Context, target string, task client.Task) error {
	var err error
	if target == "" {
		_, err = w.client.Submit(ctx, task)
	} else {
		_, err = w.client.SubmitTo(ctx, target, task)
	}
	return err
}

func (a *app) mutateAssignment(_ http.ResponseWriter, r *http.Request, _ *mutationContext) error {
	id, target := r.FormValue("card_id"), r.FormValue("target_agent_id")
	switch r.FormValue("mode") {
	case "plan":
		return a.workflow.PlanWithAgent(r.Context(), id, target)
	case "fix":
		return a.workflow.FixWithAgent(r.Context(), id, r.FormValue("message"), target)
	case "build":
		if r.FormValue("expected_revision") != "" {
			revision, err := strconv.Atoi(r.FormValue("expected_revision"))
			if err != nil || revision < 1 || r.FormValue("acknowledge") != "yes" {
				return errors.New("review and acknowledge a valid plan revision")
			}
			return a.workflow.ApprovePlanningWithAgent(r.Context(), id, revision, target)
		}
		return a.workflow.BuildWithAgent(r.Context(), id, target)
	default:
		return errors.New("invalid assignment mode")
	}
}
