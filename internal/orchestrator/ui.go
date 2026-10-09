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
	"slices"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

//go:embed assets/*
var uiAssets embed.FS

// uiSnapshot is shared by the full page and SSE, so both show the same guards.
type uiSnapshot struct {
	projects []Project
	cards    []Card
	workers  []presence.Record
	attempts []Attempt
	message  string
}

func loadUISnapshot(ctx context.Context, s *Store, wf *Workflow) (uiSnapshot, error) {
	var snap uiSnapshot
	var err error
	if snap.projects, err = s.Projects(); err != nil {
		return snap, fmt.Errorf("load projects: %w", err)
	}
	if snap.cards, err = s.Cards(""); err != nil {
		return snap, fmt.Errorf("load cards: %w", err)
	}
	snap.message = "Live activity is not connected."
	if wf != nil {
		presenceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		snap.workers, err = wf.Workers(presenceCtx)
		cancel()
		snap.message = "Live activity · connected via server events."
		if err != nil {
			snap.message = "Agent presence unavailable; activity will retry."
		}
		snap.attempts, err = wf.Attempts()
		if err != nil {
			return snap, fmt.Errorf("load attempt history: %w", err)
		}
	}
	return snap, nil
}

// renderFragments returns only stable patch roots, never the shell or open forms.
func renderFragments(ctx context.Context, s *Store, wf *Workflow, csrf string) ([]templ.Component, error) {
	snap, err := loadUISnapshot(ctx, s, wf)
	if err != nil {
		return nil, err
	}
	return []templ.Component{boardContent(csrf, snap.projects, snap.cards, snap.attempts), activityPanel(snap.workers, snap.attempts, snap.message), notification("")}, nil
}

// Snapshot renders HTML for a parent's Datastar patch-elements SSE frame.
func Snapshot(r *http.Request, s *Store, wf *Workflow) (string, error) {
	fragments, err := renderFragments(r.Context(), s, wf, CSRFToken(r))
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

func renderWorkflowPage(w http.ResponseWriter, r *http.Request, s *Store, wf *Workflow) {
	snap, err := loadUISnapshot(r.Context(), s, wf)
	if err != nil {
		http.Error(w, "Unable to load workspace", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page(CSRFToken(r), snap.projects, snap.cards, s.NATSConfig(), snap.workers, snap.attempts, snap.message).Render(r.Context(), w)
}

func displayValue(s string) string {
	if s == "" {
		return "Not reported"
	}
	return s
}
func activityCounts(workers []presence.Record, attempts []Attempt) string {
	return fmt.Sprintf("%d agents · %d attempts", len(workers), len(attempts))
}
func inspectRecord(r presence.Record) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}
func workerAvailability(r presence.Record) string {
	for _, e := range r.Endpoints {
		if r.State == presence.Ready && e.Input && e.Ready {
			return "Available (advertised)"
		}
	}
	return displayValue(string(r.State)) + " · input not ready"
}
func workerRun(r presence.Record, attempts []Attempt) string {
	for _, a := range slices.Backward(attempts) {

		if a.WorkerID == r.Identity.Worker && a.State == "running" {
			return displayValue(a.RunID)
		}
	}
	return "None reported"
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

// The branch comes from the immutable task identity, never a reported PR URL.
func attemptBranch(a Attempt) string {
	return fmt.Sprintf("jawa/card/%s/attempt/%d", a.CardID, a.Number)
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
func laneHint(status string) string {
	switch status {
	case "Building":
		return "Drop a Todo card here to start work."
	case "Done":
		return "Verified pull requests arrive here."
	}
	return "Your next idea starts here."
}
func postAction(route string, confirm bool) string {
	action := "@post('" + route + "', {contentType:'form'})"
	if confirm {
		return "$deleteOpen = true"
	}
	return action
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
