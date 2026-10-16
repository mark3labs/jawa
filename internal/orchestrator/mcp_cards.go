package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

type mcpProject struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Provider   string    `json:"provider"`
	Repo       string    `json:"repo"`
	BaseBranch string    `json:"base_branch"`
	CreatedAt  time.Time `json:"created_at"`
}
type mcpCardSummary struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	ProjectID string    `json:"project_id"`
	Title     string    `json:"title"`
	Lane      string    `json:"lane"`
	LaneLabel string    `json:"lane_label"`
	Position  int       `json:"position"`
	Archived  bool      `json:"archived"`
	Status    string    `json:"status"`
	Tone      string    `json:"tone"`
	Working   bool      `json:"working"`
	Agent     string    `json:"agent,omitempty"`
	PRURL     string    `json:"pr_url,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   string    `json:"version"`
}
type mcpCard struct {
	Actions     []mcpAction      `json:"actions"`
	Unavailable []mcpUnavailable `json:"unavailable,omitempty"`
	mcpCardSummary
	Description string       `json:"description,omitempty"`
	IssueURL    string       `json:"issue_url,omitempty"`
	AutoMerge   string       `json:"auto_merge"`
	PR          *mcpPR       `json:"pr,omitempty"`
	Blocker     *mcpBlocker  `json:"blocker,omitempty"`
	Notes       []string     `json:"notes,omitempty"`
	Planning    *mcpPlanning `json:"planning,omitempty"`
}
type mcpPR struct {
	URL         string `json:"url"`
	Check       string `json:"check,omitempty"`
	CheckLabel  string `json:"check_label,omitempty"`
	ManualReady bool   `json:"manual_ready,omitempty"`
}
type mcpBlocker struct {
	Reason string `json:"reason"`
	Queued bool   `json:"guidance_queued"`
}
type mcpPlanning struct {
	State            string   `json:"state"`
	Label            string   `json:"label"`
	Revision         int      `json:"revision"`
	ApprovedRevision int      `json:"approved_revision,omitempty"`
	Spec             string   `json:"spec,omitempty"`
	Questions        []string `json:"questions,omitempty"`
	CanApprove       bool     `json:"can_approve"`
}

var errMCPCardNotFound = errors.New("card not found")

func (m *mcpService) snapshot(ctx context.Context) (pageData, error) {
	snap, err := loadUISnapshot(ctx, m.s, m.w)
	return pageData{uiSnapshot: snap}, err
}

// projectCard is an explicit allowlist of board/plan fields. Never serialize
// storage records: those also carry private execution and discussion content.
func projectCard(d pageData, c Card) mcpCard {
	out := mcpCard{
		ID: c.ID, Key: cardKey(d.projects, c), ProjectID: c.ProjectID, Title: c.Title, Lane: c.Status, LaneLabel: laneName(c.Status), Position: c.Position, Archived: c.Archived, Status: "Todo", Tone: "idle", Agent: d.cardAgent(c), UpdatedAt: d.cardActivity(c),
		Description: c.Description, IssueURL: safeActivityURL(c.IssueURL), AutoMerge: c.AutoMergeMode,
	}
	if out.AutoMerge == "" {
		out.AutoMerge = "inherit"
	}
	latest := latestAttempt(c.ID, d.attempts)
	if c.Status == "Planning" {
		out.Status = planningAssignmentLabel(d, c.ID)
		out.Tone = planningTone(planningState(d, c.ID))
		if detail, ok := d.planning[c.ID]; ok {
			out.Working = planningWorking(detail)
		}
	} else if len(latest) > 0 {
		a := latest[0]
		out.Status, out.Tone, out.Working = d.runLabel(a), runTone(a), attemptWorking(a)
		if d.prChecks[c.ID].State == "merged" && a.Ready {
			out.Status, out.Tone = "Merged", "ready"
		}
	} else if c.Status == "Building" || c.Status == "Fix" {
		out.Status, out.Tone = "Not started", "queued"
	}
	if len(latest) > 0 {
		a := latest[0]
		out.PRURL = cardPRURL(c, a)
		if out.PRURL != "" {
			check := d.prChecks[c.ID]
			_, manual := d.readyOverrides[a.ID]
			out.PR = &mcpPR{URL: out.PRURL, Check: check.State, CheckLabel: prCheckLabel(check), ManualReady: manual}
		}
	}
	if b, ok := d.blocker(c); ok {
		out.Blocker = &mcpBlocker{Reason: b.Reason, Queued: b.Queued}
	}
	if note := d.automaticResumeNote(c); note != "" {
		out.Notes = append(out.Notes, note)
	}
	if note, _ := d.cardDiagnostic(c); note != "" {
		out.Notes = append(out.Notes, note)
	}
	if detail, ok := d.planning[c.ID]; ok {
		out.Planning = &mcpPlanning{State: planningDisplayState(detail), Label: planningAssignmentLabel(d, c.ID), Revision: detail.Session.Revision, ApprovedRevision: detail.Session.ApprovedRevision, Spec: detail.Session.Spec, Questions: detail.Questions, CanApprove: detail.CanApprove}
	}
	out.Actions, out.Unavailable = cardActions(d, c)
	b, _ := json.Marshal(out)
	hash := sha256.Sum256(b)
	out.Version = hex.EncodeToString(hash[:])[:16]
	return out
}
func (m *mcpService) card(ctx context.Context, id string) (mcpCard, error) {
	d, err := m.snapshot(ctx)
	if err != nil {
		return mcpCard{}, err
	}
	c, ok := selectedCard(d.cards, id)
	if !ok {
		return mcpCard{}, errMCPCardNotFound
	}
	return projectCard(d, c), nil
}
