package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"net/http"
	"slices"
)

// Agent routing is operator policy, not a CLI override: it lives outside the
// dashboard: prefix so Reset overrides keeps it.
const agentRoutingKey = "agent-routing"

// Work modes for routing. Fix is a build: it pushes to an existing PR.
const (
	routePlan  = "plan"
	routeBuild = "build"
)

// AgentRoles holds explicit per-agent restrictions. Agents without an entry
// may do both kinds of work.
type AgentRoles struct {
	Plan  bool `json:"plan"`
	Build bool `json:"build"`
}

type AgentRouting struct {
	Agents map[string]AgentRoles `json:"agents,omitempty"`
}

func (r AgentRouting) Allows(agent, mode string) bool {
	roles, ok := r.Agents[agent]
	if !ok {
		return true
	}
	if mode == routePlan {
		return roles.Plan
	}
	return roles.Build
}

// Restricted reports whether some agent may not take mode work. The shared
// queue cannot exclude a consumer, so restricted work is always targeted.
func (r AgentRouting) Restricted(mode string) bool {
	for id := range r.Agents {
		if !r.Allows(id, mode) {
			return true
		}
	}
	return false
}

func (r AgentRouting) validate() error {
	for id := range r.Agents {
		if !validTargetID(id) {
			return errors.New("invalid agent ID in agent roles")
		}
	}
	return nil
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func loadAgentRouting(q rowQuerier) (AgentRouting, error) {
	var raw string
	err := q.QueryRow(`SELECT value FROM settings WHERE key=?`, agentRoutingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentRouting{}, nil
	}
	if err != nil {
		return AgentRouting{}, err
	}
	var r AgentRouting
	if err = json.Unmarshal([]byte(raw), &r); err != nil {
		return AgentRouting{}, fmt.Errorf("decode agent routing: %w", err)
	}
	return r, nil
}

func (s *Store) AgentRouting() (AgentRouting, error) { return loadAgentRouting(s.db) }

func (s *Store) SaveAgentRouting(r AgentRouting) error {
	if err := r.validate(); err != nil {
		return err
	}
	// Only restrictions are stored; an all-enabled entry is the default.
	clean := AgentRouting{Agents: map[string]AgentRoles{}}
	for id, roles := range r.Agents {
		if !roles.Plan || !roles.Build {
			clean.Agents[id] = roles
		}
	}
	payload, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	tx, err := s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, agentRoutingKey, string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

// routedTarget keeps an inherited target only while its agent may still do
// mode work; otherwise the work is rerouted.
func routedTarget(r AgentRouting, target, mode string) string {
	if target != "" && !r.Allows(target, mode) {
		return ""
	}
	return target
}

func routeModeLabel(mode string) string {
	if mode == routePlan {
		return "planning"
	}
	return "building"
}

// pickRoutedAgent chooses an eligible connected agent, preferring available
// ones. Hashing the task ID spreads load and keeps the choice stable across
// publish retries.
func pickRoutedAgent(r AgentRouting, choices []AgentChoice, mode, taskID string) (string, error) {
	var ready, busy []string
	for _, c := range choices {
		if !c.Selectable || !r.Allows(c.ID, mode) {
			continue
		}
		if c.Availability == "Available" {
			ready = append(ready, c.ID)
		} else {
			busy = append(busy, c.ID)
		}
	}
	pool := ready
	if len(pool) == 0 {
		pool = busy
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("no connected agent is enabled for %s", routeModeLabel(mode))
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(taskID))
	return pool[int(h.Sum32()%uint32(len(pool)))], nil
}

// resolveRoute returns the agent that must receive untargeted work, or "" to
// use the shared queue when no agent is restricted for mode.
func (w *Workflow) resolveRoute(ctx context.Context, mode, taskID string) (string, error) {
	r, err := w.s.AgentRouting()
	if err != nil || !r.Restricted(mode) {
		return "", err
	}
	choices, err := w.choices(ctx)
	if err != nil {
		return "", err
	}
	return pickRoutedAgent(r, choices, mode, taskID)
}

// routingAgentIDs lists connected agents plus any with saved routing, so a
// restriction stays visible while its agent is offline.
func routingAgentIDs(r AgentRouting, choices []AgentChoice) []string {
	seen := map[string]bool{}
	for _, c := range choices {
		seen[c.ID] = true
	}
	for id := range r.Agents {
		seen[id] = true
	}
	return slices.Sorted(maps.Keys(seen))
}

// routeAttempt pins untargeted build or fix work to an eligible agent before
// its first publish. The target is persisted first so a retried publish after
// an ambiguous broker error reaches the same agent.
func (w *Workflow) routeAttempt(ctx context.Context, a *Attempt) error {
	if a.TargetAgentID != "" {
		return nil
	}
	target, err := w.resolveRoute(ctx, routeBuild, a.TaskID)
	if err != nil || target == "" {
		return err
	}
	res, err := w.s.db.Exec(`UPDATE workflow_attempts SET target_agent_id=? WHERE id=? AND published=0 AND target_agent_id=''`, target, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("attempt changed while routing; will retry")
	}
	a.TargetAgentID = target
	return nil
}

type routingRow struct {
	ID, Detail  string
	Connected   bool
	Plan, Build bool
}

func (d pageData) routingRows() []routingRow {
	choices := agentChoices(slices.Clone(d.agents))
	byID := map[string]AgentChoice{}
	for _, c := range choices {
		byID[c.ID] = c
	}
	var rows []routingRow
	for _, id := range routingAgentIDs(d.routing, choices) {
		row := routingRow{ID: id, Detail: "Offline", Plan: d.routing.Allows(id, routePlan), Build: d.routing.Allows(id, routeBuild)}
		if c, ok := byID[id]; ok {
			row.Connected, row.Detail = true, c.Provider+" / "+c.Model+" · "+c.Availability
		}
		rows = append(rows, row)
	}
	return rows
}

// mutateSettingsRouting replaces the routing policy for the agents the form
// listed. Saved restrictions for agents missing from the form are kept.
func (a *app) mutateSettingsRouting(_ http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	current, err := a.s.AgentRouting()
	if err != nil {
		return err
	}
	next := AgentRouting{Agents: maps.Clone(current.Agents)}
	if next.Agents == nil {
		next.Agents = map[string]AgentRoles{}
	}
	for _, id := range r.Form["agent"] {
		if !validTargetID(id) {
			return errors.New("invalid agent ID in agent roles")
		}
		roles := AgentRoles{}
		// switchField posts "true" before its hidden "false"; the first value wins.
		for _, field := range []struct {
			name string
			dst  *bool
		}{{"plan:" + id, &roles.Plan}, {"build:" + id, &roles.Build}} {
			switch r.Form.Get(field.name) {
			case "true":
				*field.dst = true
			case "false":
			default:
				return fmt.Errorf("agent roles for %s must be true or false", id)
			}
		}
		next.Agents[id] = roles
	}
	if err = a.s.SaveAgentRouting(next); err != nil {
		return err
	}
	ctx.effects.flash = "Agent roles saved. They apply to work queued from now on."
	return nil
}
