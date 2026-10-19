package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/a-h/templ"
	"github.com/axadrn/shadcn-templ/v2/components/dropdownmenu"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/bonnie/presence"
)

//go:embed assets/*
var uiAssets embed.FS

// uiSnapshot is shared by full pages and SSE, so both show the same guards.
type uiSnapshot struct {
	readyOverrides map[string]readyOverride
	pendingResumes map[string]bool
	autoResumes    map[string]autoResumeStatus
	autoMerges     map[string]autoMergeStatus
	blockers       map[string]BlockerGuidance // open build blockers by card
	projects       []Project
	cards          []Card
	agents         []presence.Record
	planning       map[string]PlanningDetail
	prChecks       map[string]PRCheck
	runs           []RunRecord
	attempts       []Attempt
	nats           NATSConfig
	natsConfigured bool
	presence       bool // false when presence discovery failed
	now            time.Time
}

// pageData is everything a view needs to render. Views never query storage.
type pageData struct {
	apiKeys            []APIKey
	workflowDiscussion []WorkflowExchange
	planningChat       *planningChatDetail
	detail             *runDetail
	csrf               string
	v                  viewParams
	dashboard          map[string]string
	defaults           map[string]string
	uiSnapshot
}

func loadUISnapshot(ctx context.Context, s *Store, wf *Workflow) (uiSnapshot, error) {
	snap := uiSnapshot{now: time.Now(), nats: s.NATSConfig(), presence: true}
	var err error
	if snap.natsConfigured, err = s.NATSConfigured(); err != nil {
		return snap, fmt.Errorf("load NATS setup: %w", err)
	}
	if !snap.natsConfigured {
		snap.nats.Username = "worker"
	}
	if snap.readyOverrides, err = s.readyOverrides(); err != nil {
		return snap, err
	}
	snap.pendingResumes = make(map[string]bool)
	rows, err := s.db.Query(`SELECT DISTINCT card_id FROM failed_resumes WHERE status='pending'`)
	if err != nil {
		return snap, err
	}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return snap, err
		}
		snap.pendingResumes[id] = true
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return snap, err
	}
	if snap.autoMerges, err = s.autoMergeStatuses(); err != nil {
		return snap, err
	}
	if snap.autoResumes, err = s.autoResumeStatuses(); err != nil {
		return snap, err
	}
	if snap.blockers, err = s.blockerGuidance(); err != nil {
		return snap, fmt.Errorf("load blockers: %w", err)
	}
	if snap.projects, err = s.Projects(); err != nil {
		return snap, fmt.Errorf("load projects: %w", err)
	}
	if snap.cards, err = s.Cards(""); err != nil {
		return snap, fmt.Errorf("load cards: %w", err)
	}
	snap.planning = make(map[string]PlanningDetail)
	for _, c := range snap.cards {
		detail, planningErr := s.PlanningDetail(c.ID)
		if planningErr == nil {
			snap.planning[c.ID] = detail
		} else if !errors.Is(planningErr, sql.ErrNoRows) {
			return snap, fmt.Errorf("load planning detail: %w", planningErr)
		}
	}
	snap.prChecks = make(map[string]PRCheck)
	if wf != nil {
		for _, c := range snap.cards {
			if c.Status != "Done" {
				continue
			}
			check, checkErr := wf.CardPRCheck(c.ID)
			if checkErr == nil {
				snap.prChecks[c.ID] = check
			} else if !errors.Is(checkErr, sql.ErrNoRows) {
				return snap, fmt.Errorf("load PR check: %w", checkErr)
			}
		}
		presenceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		snap.agents, err = wf.Agents(presenceCtx)
		cancel()
		snap.presence = err == nil
		if snap.attempts, err = wf.Attempts(); err != nil {
			return snap, fmt.Errorf("load attempt history: %w", err)
		}
	}
	if snap.runs, err = s.runHistory(ctx, snap.attempts); err != nil {
		return snap, err
	}
	return snap, nil
}

func loadPageData(ctx context.Context, s *Store, wf *Workflow, csrf string, v viewParams, runtimeDefaults ...map[string]string) (pageData, error) {
	snap, err := loadUISnapshot(ctx, s, wf)
	if err != nil {
		return pageData{}, err
	}
	if v.View == viewBoard {
		if p, ok := selectedProject(snap.projects, v.Project); ok {
			v.Project = p.ID
		}
	}
	dashboard, err := s.DashboardOverrides()
	if err != nil {
		return pageData{}, fmt.Errorf("load dashboard settings: %w", err)
	}
	defaults := map[string]string{}
	if len(runtimeDefaults) > 0 {
		maps.Copy(defaults, runtimeDefaults[0])
	}
	d := pageData{csrf: csrf, v: v, dashboard: dashboard, defaults: defaults, uiSnapshot: snap}
	if v.View == viewPlanning {
		d.workflowDiscussion, err = s.workflowDiscussion(v.Card)
		if err == nil {
			if wf != nil {
				if _, ok := d.planning[v.Card]; ok {
					if record, found := latestPinnedPlanningRun(d.runs, v.Card); found {
						chat := planningChatDetail{Record: record, MessageID: uuid.NewString()}
						chat.Messages = wf.chatMessages(record)
						d.planningChat = &chat
					}
				}
			}
		}
	}
	if err == nil && v.View == viewSettings {
		d.apiKeys, err = s.APIKeys(ctx)
	}
	return d, err
}

// renderFragments returns only stable patch roots, never the shell or open
// forms: the active view's root, the sidebar navigation, the agent chip and,
// last, the (empty) notice. Live updates omit that last fragment.
func renderFragments(ctx context.Context, s *Store, wf *Workflow, csrf string, v viewParams, runtimeDefaults ...map[string]string) ([]templ.Component, error) {
	d, err := loadPageData(ctx, s, wf, csrf, v, runtimeDefaults...)
	if err != nil {
		return nil, err
	}
	var out []templ.Component
	switch v.View {
	case viewArchive:
		out = append(out, archiveContent(d))
	case viewPlanning:
		out = append(out, planningContent(d))
	case viewBoard:
		out = append(out, boardContent(d))
	case viewRuns:
		out = append(out, runsContent(d))
	case viewAgents:
		out = append(out, agentsContent(d))
	case viewSettings:
		out = append(out, settingsContent(d))
	}
	return append(out, sidebarNav(d), agentStatus(d), notification("", "")), nil
}

// Snapshot renders HTML for a Datastar patch-elements SSE frame.
func Snapshot(r *http.Request, s *Store, wf *Workflow, v viewParams, runtimeDefaults ...map[string]string) (string, error) {
	fragments, err := renderFragments(r.Context(), s, wf, CSRFToken(r), v, runtimeDefaults...)
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

func renderWorkflowPage(w http.ResponseWriter, r *http.Request, s *Store, wf *Workflow, v viewParams, runtimeDefaults ...map[string]string) {
	d, err := loadPageData(r.Context(), s, wf, CSRFToken(r), v, runtimeDefaults...)
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

// Display the immutable assigned ref, not a guess from the current card title.
func attemptBranch(a Attempt) string {
	branch, err := assignedBranch(a)
	if err != nil {
		return "Not reported"
	}
	return branch
}

// cardPRURL displays repair assignment without treating it as completion evidence.
func cardPRURL(c Card, a Attempt) string {
	pr := a.PRURL
	if pr == "" && c.Status == "Fix" {
		pr = a.RepairPRURL
	}
	return safeActivityURL(pr)
}

func safeActivityURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

func cardNeedsReconciliation(c Card, attempts []Attempt) bool {
	latest := latestAttempt(c.ID, attempts)
	return len(latest) > 0 && !latest[0].Ready && latest[0].Error == executionConflictNotice
}

// Match workflow guards for presentation; the server remains authoritative.
func canAction(c Card, attempts []Attempt, action string) bool {
	active := cardHasActiveAttempt(c.ID, attempts)
	latest := latestAttempt(c.ID, attempts)
	legacy := len(latest) == 0
	terminal := !legacy && terminalFailure(latest[0])
	switch action {
	case "force-ready":
		return forceReadyReason(c, attempts, false) == ""
	case "start":
		return c.Status == "Building" && legacy && !active
	case "resume":
		return (c.Status == "Building" || c.Status == "Fix") && !legacy && !active && executionFailure(latest[0])
	case "retry":
		return (c.Status == "Building" || c.Status == "Fix") && terminal && !active
	case "fix":
		return c.Status == "Done" && !legacy && latest[0].Ready && latest[0].State == "ready" && latest[0].PRURL != "" && !active
	case "reset":
		return resetReason(c, attempts, false) == ""
	case "delete":
		return !active || (!legacy && !latest[0].Ready && latest[0].Error == executionConflictNotice)
	case "cancel":
		if (c.Status != "Building" && c.Status != "Fix") || len(latest) == 0 {
			return false
		}
		a := latest[0]
		return a.AgentID != "" && a.RunID != "" && a.RemoteAttemptID != "" && (a.State == "running" || a.State == "waiting" || a.State == "interrupted" || a.RunState == "waiting")
	case "force-stop":
		if c.Status == "Planning" || legacy {
			return false
		}
		for _, a := range attempts {
			if a.CardID == c.ID && forceStoppable(a, a.ID == latest[0].ID) {
				return true
			}
		}
		return false
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

func planningState(d pageData, cardID string) string {
	if detail, ok := d.planning[cardID]; ok {
		return planningDisplayState(detail)
	}
	return "queued"
}
func planningDisplayState(detail PlanningDetail) string {
	if detail.Session.State == "running" && detail.Session.Error == resumePendingNotice {
		return "resuming"
	}
	return detail.Session.State
}
func planningLabel(state string) string {
	switch state {
	case "resuming":
		return "Resuming…"
	case "running", "submitted":
		return "Investigating"
	case "questions":
		return "Needs input"
	case "ready":
		return "Ready"
	case "failed":
		return "Failed"
	case "approved":
		return "Approved"
	case "cancelled":
		return "Cancelled"
	default:
		return "Queued"
	}
}
func planningTone(state string) string {
	switch state {
	case "resuming":
		return "resuming"
	case "running", "submitted":
		return "running"
	case "questions":
		return "blocked"
	case "ready", "approved":
		return "ready"
	case "failed":
		return "failed"
	default:
		return "queued"
	}
}
func selectedCard(cards []Card, id string) (Card, bool) {
	for _, c := range cards {
		if c.ID == id {
			return c, true
		}
	}
	return Card{}, false
}
func planningCanReply(detail PlanningDetail) bool {
	return detail.InteractionToken != "" && (detail.Session.State == "questions" || detail.Session.State == "ready")
}

func planningInteractionLabel(status string) string {
	switch status {
	case "pending":
		return "Waiting for your input"
	case "queued":
		return "Reply queued — waiting for agent suspension/delivery"
	case "answered":
		return "Reply dispatched"
	case "cancelled":
		return "Closed"
	default:
		return status
	}
}

const conflictFixMessage = "Resolve merge conflicts with the current base branch, run project tests, and push fixes to the existing pull request."

// blocker returns the open blocker of a card's latest Building/Fix run.
func (d pageData) blocker(c Card) (BlockerGuidance, bool) {
	b, ok := d.blockers[c.ID]
	if !ok || (c.Status != "Building" && c.Status != "Fix") {
		return b, false
	}
	latest := latestAttempt(c.ID, d.attempts)
	return b, len(latest) > 0 && latest[0].TaskID == b.TaskID && !latest[0].Ready
}

// canGuide matches GuideBlocked: only an unanswered blocker accepts guidance.
func (d pageData) canGuide(c Card) bool {
	b, ok := d.blocker(c)
	return ok && !b.Queued && !b.Cancelling && latestAttempt(c.ID, d.attempts)[0].State == "blocked"
}

func guideDialogAction(card, token string) string { return dialogOpen("guide", card, "") }

func fixDialogAction(card, message string) string {
	extra := ""
	if message != "" {
		extra = "conflict"
	}
	return dialogOpen("fix", card, extra)
}

// PR check evidence is independent of the historical ready badge.
func prCheckLabel(check PRCheck) string {
	switch check.State {
	case "pending":
		return "Merge in progress. Closing the dialog does not cancel remote work."
	case "uncertain":
		return "Merge outcome uncertain. Refresh ready PRs to reconcile; do not retry blindly."
	case "checking":
		return "Checking PR status… (queued or in progress)"
	case "clear":
		return "No merge conflicts detected."
	case "conflict":
		return "Pull request has merge conflicts."
	case "unknown":
		return "PR merge status is unknown. Try checking again."
	case "error":
		return "PR check failed. Try checking again."
	case "merged":
		return "Pull request is merged."
	case "closed":
		return "Pull request is closed without merging."
	default:
		return "PR status has not been checked yet."
	}
}

func (d pageData) automaticResumeNote(c Card) string {
	var a Attempt
	if c.Status == "Planning" {
		detail, ok := d.planning[c.ID]
		if !ok || detail.Session.State != "failed" || len(detail.Turns) == 0 {
			return ""
		}
		t, _ := latestPlanningTurn(detail)
		status, ok := d.autoResumes[t.ID]
		if !ok || !detail.CanResume || status.token != detail.ResumeToken {
			return ""
		}
		return automaticResumeStatusNote(status)
	} else {
		latest := latestAttempt(c.ID, d.attempts)
		if (c.Status != "Building" && c.Status != "Fix") || len(latest) == 0 || !executionFailure(latest[0]) {
			return ""
		}
		a = latest[0]
	}
	status, ok := d.autoResumes[a.TaskID]
	if !ok || status.token != resumeToken(a) {
		return ""
	}
	return automaticResumeStatusNote(status)
}

func automaticResumeStatusNote(status autoResumeStatus) string {
	switch status.state {
	case "scheduled":
		return fmt.Sprintf("Automatic resume %d/5 scheduled for %s", status.count+1, time.UnixMilli(status.due).UTC().Format(time.RFC3339))
	case "exhausted":
		return "Automatic resume budget exhausted (5/5). Manual resume or fresh Retry remains available when safe."
	case "stopped":
		return "Automatic resume stopped after host rejection. Manual recovery remains available when safe."
	}
	return ""
}

// latestPlanningTurn selects current work independently of storage/slice ordering.
func latestPlanningTurn(detail PlanningDetail) (PlanningTurn, bool) {
	var latest PlanningTurn
	found := false
	for _, turn := range detail.Turns {
		if turn.SessionID != "" && turn.SessionID != detail.Session.ID {
			continue
		}
		if !found || turn.Number > latest.Number {
			latest, found = turn, true
		}
	}
	return latest, found
}

func (d pageData) cardAgent(c Card) string {
	if c.Status == "Planning" {
		detail, ok := d.planning[c.ID]
		if !ok {
			return ""
		}
		switch detail.Session.State {
		case "submitted", "running", "questions", "ready":
			turn, ok := latestPlanningTurn(detail)
			if ok && (turn.State == "submitted" || turn.State == "running" || turn.State == "waiting") {
				return turn.AgentID
			}
		}
		return ""
	}
	if latest := latestAttempt(c.ID, d.attempts); len(latest) > 0 && attemptActive(latest[0]) {
		return latest[0].AgentID
	}
	return ""
}

func (d pageData) cardDiagnostic(c Card) (string, string) {
	if c.Status == "Planning" {
		detail, ok := d.planning[c.ID]
		if !ok || detail.Session.Error == resumePendingNotice {
			return "", ""
		}
		return detail.Session.Error, planningTone(planningDisplayState(detail))
	}
	if latest := latestAttempt(c.ID, d.attempts); len(latest) > 0 && !attemptActive(latest[0]) && runTone(latest[0]) != "ready" {
		return latest[0].Error, runTone(latest[0])
	}
	return "", ""
}

// cardActivity is activity time, including retained execution and current planning.
func (d pageData) cardActivity(c Card) time.Time {
	updated := cardUpdated(c, d.attempts)
	if detail, ok := d.planning[c.ID]; ok {
		if detail.Session.UpdatedAt.After(updated) {
			updated = detail.Session.UpdatedAt
		}
		for _, turn := range detail.Turns {
			if (turn.SessionID == "" || turn.SessionID == detail.Session.ID) && turn.UpdatedAt.After(updated) {
				updated = turn.UpdatedAt
			}
		}
	}
	return updated
}

func (d pageData) canReset(c Card) bool {
	return d.resetReason(c) == ""
}
func (d pageData) resetReason(c Card) string {
	return resetReason(c, d.attempts, d.pendingResumes[c.ID])
}

func (d pageData) canForceReady(c Card) bool {
	return forceReadyReason(c, d.attempts, d.pendingResumes[c.ID]) == ""
}
func (d pageData) readyDialogAction(c Card) string { return dialogOpen("ready", c.ID, "") }
func (d pageData) runLabel(a Attempt) string {
	if _, ok := d.readyOverrides[a.ID]; ok && a.Ready {
		return "Ready (manual)"
	}
	return runLabel(a)
}

func assignmentAction(c Card, mode string) string { return dialogOpen("assign", c.ID, mode) }

func planningAssignmentLabel(d pageData, id string) string {
	if detail, ok := d.planning[id]; ok && len(detail.Turns) > 0 {
		turn := detail.Turns[len(detail.Turns)-1]
		if turn.AgentID == "" && turn.TargetAgentID != "" && (turn.State == "queued" || turn.State == "submitted") {
			return "Queued for " + turn.TargetAgentID
		}
	}
	return planningLabel(planningState(d, id))
}

func (d pageData) canMerge(c Card) bool {
	return mergeEligible(c, d.attempts, d.prChecks[c.ID], false)
}
func (d pageData) mergeDialogAction(c Card) string { return dialogOpen("merge", c.ID, "") }

func menuTrigger(ctx context.Context, extra templ.Attributes) templ.Attributes {
	attrs := dropdownmenu.Trigger(ctx)
	delete(attrs, "type")
	maps.Copy(attrs, extra)
	return attrs
}
func routeSlug(route string) string { return strings.ReplaceAll(strings.Trim(route, "/"), "/", "-") }
func dsForm(route, sig string) templ.Attributes {
	return templ.Attributes{"data-indicator:_" + sig: true, "data-on:submit__prevent": fmt.Sprintf("!$_%s && @post('%s', {contentType:'form'})", sig, route)}
}
