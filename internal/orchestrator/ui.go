package orchestrator

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/a-h/templ"
	"net/http"
	"net/url"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

//go:embed assets/*
var uiAssets embed.FS

// uiSnapshot is shared by full pages and SSE, so both show the same guards.
type uiSnapshot struct {
	projects []Project
	cards    []Card
	workers  []presence.Record
	attempts []Attempt
	nats     NATSConfig
	presence bool // false when presence discovery failed
	now      time.Time
}

// pageData is everything a view needs to render. Views never query storage.
type pageData struct {
	csrf string
	v    viewParams
	uiSnapshot
}

func loadUISnapshot(ctx context.Context, s *Store, wf *Workflow) (uiSnapshot, error) {
	snap := uiSnapshot{now: time.Now(), nats: s.NATSConfig(), presence: true}
	var err error
	if snap.projects, err = s.Projects(); err != nil {
		return snap, fmt.Errorf("load projects: %w", err)
	}
	if snap.cards, err = s.Cards(""); err != nil {
		return snap, fmt.Errorf("load cards: %w", err)
	}
	if wf != nil {
		presenceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		snap.workers, err = wf.Workers(presenceCtx)
		cancel()
		snap.presence = err == nil
		if snap.attempts, err = wf.Attempts(); err != nil {
			return snap, fmt.Errorf("load attempt history: %w", err)
		}
	}
	return snap, nil
}

func loadPageData(ctx context.Context, s *Store, wf *Workflow, csrf string, v viewParams) (pageData, error) {
	snap, err := loadUISnapshot(ctx, s, wf)
	if err != nil {
		return pageData{}, err
	}
	if v.View == viewBoard {
		if p, ok := selectedProject(snap.projects, v.Project); ok {
			v.Project = p.ID
		}
	}
	return pageData{csrf: csrf, v: v, uiSnapshot: snap}, nil
}

// renderFragments returns only stable patch roots, never the shell or open
// forms: the active view's root, the sidebar navigation, the agent chip and,
// last, the (empty) notice. Live updates omit that last fragment.
func renderFragments(ctx context.Context, s *Store, wf *Workflow, csrf string, v viewParams) ([]templ.Component, error) {
	d, err := loadPageData(ctx, s, wf, csrf, v)
	if err != nil {
		return nil, err
	}
	var out []templ.Component
	switch v.View {
	case viewBoard:
		out = append(out, boardContent(d))
	case viewRuns:
		out = append(out, runsContent(d))
	case viewAgents:
		out = append(out, agentsContent(d))
	}
	return append(out, sidebarNav(d), agentStatus(d), notification("", "")), nil
}

// Snapshot renders HTML for a Datastar patch-elements SSE frame.
func Snapshot(r *http.Request, s *Store, wf *Workflow, v viewParams) (string, error) {
	fragments, err := renderFragments(r.Context(), s, wf, CSRFToken(r), v)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	for _, fragment := range fragments {
		if err := fragment.Render(r.Context(), &out); err != nil {
			return "", err
		}
	}
	return out.String(), nil
}

func renderWorkflowPage(w http.ResponseWriter, r *http.Request, s *Store, wf *Workflow, v viewParams) {
	d, err := loadPageData(r.Context(), s, wf, CSRFToken(r), v)
	if err != nil {
		http.Error(w, "Unable to load workspace", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = appPage(d).Render(r.Context(), w)
}

func inspectRecord(r presence.Record) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}
func latestAttempt(cardID string, attempts []Attempt) []Attempt {
	var latest *Attempt
	for i := range attempts {
		a := &attempts[i]
		if a.CardID == cardID && (latest == nil || a.Number > latest.Number) {
			latest = a
		}
	}
	if latest == nil {
		return nil
	}
	return []Attempt{*latest}
}

// Display the immutable assigned ref, not a guess from the current card title.
func attemptBranch(a Attempt) string {
	branch, err := assignedBranch(a)
	if err != nil {
		return "Not reported"
	}
	return branch
}
func safeActivityURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

// Match workflow guards for presentation; the server remains authoritative.
func canAction(c Card, attempts []Attempt, action string) bool {
	active := false
	for _, a := range attempts {
		if a.CardID != c.ID {
			continue
		}
		for _, state := range []string{a.State, a.RunState} {
			if state == "queued" || state == "submitted" || state == "running" || state == "waiting" {
				active = true
			}
		}
	}
	latest := latestAttempt(c.ID, attempts)
	legacy := len(latest) == 0
	terminal := !legacy && terminalFailure(latest[0])
	switch action {
	case "start":
		return c.Status == "Building" && legacy && !active
	case "retry":
		return c.Status == "Building" && terminal && !active
	case "reset":
		return c.Status != "Todo" && !active && (legacy || terminal)
	case "delete":
		return !active
	}
	return false
}

func passwordAutocomplete(kind string) string {
	if kind == "setup" {
		return "new-password"
	}
	return "current-password"
}
func passwordMinLength(kind string) string {
	if kind == "setup" {
		return "12"
	}
	return "1"
}

func actionReason(c Card, attempts []Attempt, action string) string {
	for _, a := range attempts {
		if a.CardID != c.ID {
			continue
		}
		for _, state := range []string{a.State, a.RunState} {
			if state == "queued" || state == "submitted" || state == "running" || state == "waiting" {
				return "Active attempt — wait for work to finish."
			}
		}
	}
	if action == "reset" && c.Status == "Todo" {
		return "Already in Todo."
	}
	return "Reset requires a failed or blocked attempt."
}
