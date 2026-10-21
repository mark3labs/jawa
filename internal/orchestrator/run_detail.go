package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/a-h/templ"
	"github.com/axadrn/shadcn-templ/v2/components/badge"
	"github.com/google/uuid"
	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
	"jawa/internal/runapitoken"
	"net/http"
	"sort"
	"strings"
	"time"
)

type planningChatDetail struct {
	Record    RunRecord
	MessageID string
	Messages  []RunChatMessage
}

// planningDiscussion merges ordinary messages without exposing transport bookkeeping.
func planningDiscussion(detail PlanningDetail, chat *planningChatDetail) []PlanningMessage {
	if chat == nil {
		return detail.Discussion
	}
	return planningChatDiscussion(chat.Messages, detail.Discussion)
}

func planningChatDiscussion(chat []RunChatMessage, typed []PlanningMessage) []PlanningMessage {
	out := append([]PlanningMessage(nil), typed...)
	for _, message := range chat {
		// Interaction delivery already writes its user message to planning_messages
		// atomically with the Answer outbox. Queued messages are not there yet.
		if message.Classification == "interaction" {
			continue
		}
		out = append(out, PlanningMessage{ID: message.ID, Role: "user", Content: message.Text, CreatedAt: time.UnixMilli(message.CreatedAt).UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

type runDetail struct {
	Record      RunRecord
	Tab         string
	Observation runObservation
	Messages    []runMessage
	Chat        []RunChatMessage
	Interaction string
	MessageID   string
	Policy      runPolicy
	// CSRF is the viewer's token for forms rendered inside patch roots.
	CSRF string
	// SentAt holds acceptance times of chat messages that a delivery receipt
	// places in the snapshot, by message index.
	SentAt map[int]time.Time
}

func (d runDetail) sentLabel(i int) string {
	if t, ok := d.SentAt[i]; ok {
		return "Sent " + runTime(t)
	}
	return ""
}

// runChatEntryView is one rendered Chat message. Role is the displayed role:
// tool messages continue the assistant reply they belong to.
type runChatEntryView struct {
	Index              int
	Message            runMessage
	Role               string
	Boundary, ShowRole bool
}

func (d runDetail) chatEntries() []runChatEntryView {
	var out []runChatEntryView
	previous := ""
	for i, m := range d.Messages {
		if !m.chatVisible() {
			continue
		}
		role := m.Role
		if role == "tool" {
			role = "assistant"
		}
		out = append(out, runChatEntryView{Index: i, Message: m, Role: role, Boundary: len(out) > 0 && m.Role == "user", ShowRole: role != previous || role == "user"})
		previous = role
	}
	return out
}

func (t *runTool) result() *runTool {
	if t.Kind == "result" {
		return t
	}
	return t.Result
}

func deliveryLabel(status string) string {
	switch status {
	case "queued":
		return "Queued"
	case "dispatched":
		return "Sending"
	case "active", "completed":
		return "Delivered"
	case "failed":
		return "Failed"
	case "ambiguous":
		return "Uncertain"
	}
	return runRole(status)
}
func deliveryVariant(status string) badge.Variant {
	switch status {
	case "failed":
		return badge.VariantDestructive
	case "ambiguous":
		return badge.VariantOutline
	case "active", "completed":
		return badge.VariantDefault
	}
	return badge.VariantSecondary
}

func traceRowAttrs(row runTraceRow) templ.Attributes {
	attrs := templ.Attributes{"data-trace-entry": true, "data-trace-source": row.Source, "data-show": traceRowExpr(row.Source)}
	if row.Turn != "" {
		attrs["data-trace-turn"] = traceTurnKey(row.Turn)
	}
	return attrs
}

type runPolicy struct {
	Send, Stop         bool
	Turn, Mode, Reason string
}

func detailTab(r *http.Request) string {
	if r.FormValue("tab") == "trace" {
		return "trace"
	}
	return "chat"
}
func detailPath(a Attempt, tab string) string { return buildRun(a).path(tab) }
func (w *Workflow) attemptByID(id string) (Attempt, error) {
	return scanAttempt(w.s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, id))
}
func (w *Workflow) apiAvailable(ctx context.Context, source runObserver) bool {
	a := source.observationIdentity()
	if w.runAPI == nil {
		return false
	}
	subject, err := runapitoken.Subject("bonnie", a.AgentID)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	agents, err := w.Agents(ctx)
	if err != nil {
		return false
	}
	for _, agent := range agents {
		if agent.Identity.Agent == a.AgentID && agent.Labels["jawa.api_subject"] == subject {
			return true
		}
	}
	return false
}

// Chat acceptance is independent of remote availability and workflow state.
// Stop alone remains bound to an observed exact active turn.
func (w *Workflow) runPolicy(a Attempt, snap *bonnieclient.Snapshot, online bool) runPolicy {
	p := runPolicy{Send: true, Mode: "queue", Reason: "Message queued durably; historical work is conversational follow-up only."}
	if !a.Ready && a.RunState != "abandoned" {
		p.Mode = "steer"
	}
	if online && snap != nil && snap.RunID == a.RunID && snap.State == runtime.RunRunning && snap.TurnID != "" {
		p.Stop = true
		p.Turn = snap.TurnID
	}
	return p
}
func (a *app) detailModel(r *http.Request, attempt Attempt, obs runObservation, online bool) runDetail {
	d := runDetail{Record: buildRun(attempt), Tab: detailTab(r), Observation: obs, CSRF: CSRFToken(r)}
	d.Policy = a.workflow.runPolicy(attempt, obs.Snapshot, online && !obs.Unreachable)
	if obs.Snapshot != nil {
		d.Messages = projectRunMessages(obs.Snapshot.Messages)
	}
	a.loadChatDetail(r.Context(), &d)
	return d
}

func (a *app) historyDetailModel(r *http.Request, record RunRecord, obs runObservation, online bool) runDetail {
	if record.Mode != "Planning" {
		return a.detailModel(r, record.Build, obs, online)
	}
	d := runDetail{Record: record, Tab: detailTab(r), Observation: obs, CSRF: CSRFToken(r),
		Policy: runPolicy{Send: true, Mode: "steer", Reason: "Messages are saved before delivery. Specification approval still requires the card action."}}
	if obs.Snapshot != nil {
		d.Messages = projectRunMessages(obs.Snapshot.Messages)
	}
	a.loadChatDetail(r.Context(), &d)
	return d
}

// Exact dynamic routes pass the same authentication, body and setup guards as
// the fixed screens. Browser input can select an attempt, never a run/subject.
func (a *app) runDetailRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/runs/"), "/")
	mode := "Build"
	if len(parts) > 0 && parts[0] == "planning" {
		mode = "Planning"
		parts = parts[1:]
	}
	if len(parts) > 2 || len(parts) == 0 || !validAttemptID(parts[0]) || a.workflow == nil {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
		if action != "events" && action != "chat" && action != "stop" && action != "reconcile" {
			http.NotFound(w, r)
			return
		}
	}
	method := http.MethodGet
	if action == "chat" || action == "stop" || action == "reconcile" {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	attempt, err := a.workflow.historyByID(r.Context(), mode, parts[0])
	if err != nil || !attempt.pinned() {
		http.NotFound(w, r)
		return
	}
	if method == http.MethodPost {
		if mode == "Planning" && action == "stop" {
			a.responseError(w, r, "Use Cancel planning on the card.", http.StatusConflict)
			return
		}
		if !a.csrf(w, r) {
			return
		}
		ready, e := a.s.NATSConfigured()
		if e != nil || !ready {
			a.responseError(w, r, "NATS setup required.", http.StatusConflict)
			return
		}
		if action == "reconcile" {
			a.reconcileRunChatAction(w, r, attempt)
			return
		}
		if action == "chat" {
			a.runChatAction(w, r, attempt)
			return
		}
		a.runStopAction(w, r, attempt.Build)
		return
	}
	a.ensureCSRF(w, r)
	online := a.workflow.apiAvailable(r.Context(), attempt)
	if action == "events" {
		a.runDetailEvents(w, r, attempt, online)
		return
	}
	obs := runObservation{Unreachable: true}
	if online {
		snap, e := a.workflow.runAPI.snapshot(r.Context(), attempt)
		obs.Snapshot = snap
		obs.Unreachable = e != nil
	}
	data, e := loadPageData(r.Context(), a.s, a.workflow, CSRFToken(r), viewParams{View: viewRuns, State: "all"}, a.runtimeDefaults)
	if e != nil {
		a.responseError(w, r, "Unable to load run.", http.StatusServiceUnavailable)
		return
	}
	detail := a.historyDetailModel(r, attempt, obs, online)
	data.detail = &detail
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = appPage(data).Render(r.Context(), w)
}
func (a *app) runDetailEvents(w http.ResponseWriter, r *http.Request, attempt RunRecord, online bool) {
	var hub *runHub
	var stale runObservation
	release := func() {}
	if online {
		var err error
		hub, release, err = a.workflow.runAPI.observe(attempt)
		if err != nil {
			a.responseError(w, r, "Run observation busy; reload later.", http.StatusServiceUnavailable)
			return
		}
	}
	defer func() { release() }()
	stream := newSSEWriter(w)
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	var patches runDetailPatcher
	waited := 0
	for {
		if !a.authed(r) {
			return
		}
		fresh, e := a.workflow.historyByID(r.Context(), attempt.Mode, attempt.ID)
		if e != nil {
			return
		}
		if fresh.AgentID != attempt.AgentID || fresh.RunID != attempt.RunID || fresh.RemoteAttemptID != attempt.RemoteAttemptID {
			return
		}
		available := a.workflow.apiAvailable(r.Context(), fresh)
		obs := stale
		obs.Unreachable = true
		if hub != nil {
			obs = hub.read()
		}
		if !available && hub != nil {
			stale = obs
			release()
			hub = nil
			obs.Unreachable = true
		}
		if available && hub == nil {
			newHub, newRelease, err := a.workflow.runAPI.observe(fresh)
			if err == nil {
				hub = newHub
				release = newRelease
			}
		}
		// A reconnecting viewer gets a new hub whose first snapshot is still in
		// flight. Wait for it (bounded by the snapshot timeout) instead of
		// patching the viewer's page with that empty observation.
		if hub != nil && !obs.Observed && waited < 10 {
			waited++
		} else {
			detail := a.historyDetailModel(r, fresh, obs, available)
			frames, err := patches.frames(r.Context(), detail)
			if err != nil {
				return
			}
			for _, frame := range frames {
				if stream.write(frame) != nil {
					return
				}
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-a.workflow.ctx.Done():
			return
		case <-heartbeat.C:
			if stream.write(": heartbeat\n\n") != nil {
				return
			}
		case <-poll.C:
		}
	}
}

// runDetailPatcher sends only the patch roots that changed since the previous
// tick. Committed chat entries are append-only, so new entries are appended
// to #run-messages and existing ones are morphed only when they change, for
// example when a call's result arrives. Anything else replaces the list.
//
// A missing snapshot means the conversation is not observed (the agent is
// unreachable or a new hub is still loading), never that it is empty, so the
// transcript and Trace tables the viewer already has are left alone. Once a
// snapshot arrives the list is morphed in place, keyed by entry, preserving
// scroll position and open rows.
type runDetailPatcher struct {
	started, synced bool
	parts           map[string]string
	keys            []string
	entries         map[string]string
}

func (p *runDetailPatcher) frames(ctx context.Context, d runDetail) ([]string, error) {
	render := func(c templ.Component) (string, error) {
		var out bytes.Buffer
		err := c.Render(ctx, &out)
		return out.String(), err
	}
	observed := d.Observation.Snapshot != nil
	type patchRoot struct {
		id string
		c  templ.Component
	}
	roots := []patchRoot{{"run-detail-status", runDetailStatus(d)}, {"run-detail-controls", runDetailControls(d, d.CSRF)}}
	if d.Tab != "trace" {
		roots = append(roots, patchRoot{"run-live", runLive(d)}, patchRoot{"run-pending", runPending(d)})
	} else if observed {
		roots = append(roots, patchRoot{"run-trace", runTrace(d)})
	}
	parts := make(map[string]string, len(roots)+1)
	if html, ok := p.parts["run-trace"]; ok && !observed {
		parts["run-trace"] = html
	}
	for _, root := range roots {
		html, err := render(root.c)
		if err != nil {
			return nil, err
		}
		parts[root.id] = html
	}
	keys, entries := p.keys, p.entries
	if d.Tab != "trace" && observed {
		keys, entries = nil, make(map[string]string)
		for _, e := range d.chatEntries() {
			html, err := render(runChatEntry(d, e))
			if err != nil {
				return nil, err
			}
			keys = append(keys, e.Message.Key)
			entries[e.Message.Key] = html
		}
	}
	synced := p.synced || observed
	defer func() { p.started, p.synced, p.parts, p.keys, p.entries = true, synced, parts, keys, entries }()
	if !p.started && observed {
		full, err := render(templ.Join(runDetailContent(d), runDetailControls(d, d.CSRF)))
		if err != nil {
			return nil, err
		}
		return []string{elementsFrame(full)}, nil
	}
	var morph, appended strings.Builder
	for _, root := range roots {
		if old, sent := p.parts[root.id]; !p.started || !sent || parts[root.id] != old {
			morph.WriteString(parts[root.id])
		}
	}
	if d.Tab != "trace" && observed {
		prefix := p.synced && len(keys) >= len(p.keys)
		for i := 0; prefix && i < len(p.keys); i++ {
			prefix = keys[i] == p.keys[i]
		}
		if prefix {
			for i, key := range keys {
				if i >= len(p.keys) {
					appended.WriteString(entries[key])
				} else if entries[key] != p.entries[key] {
					morph.WriteString(entries[key])
				}
			}
		} else {
			html, err := render(runMessages(d))
			if err != nil {
				return nil, err
			}
			morph.WriteString(html)
		}
	}
	var frames []string
	if appended.Len() > 0 {
		frames = append(frames, elementsFrameMode("append", "#run-messages", appended.String()))
	}
	if morph.Len() > 0 {
		frames = append(frames, elementsFrame(morph.String()))
	}
	return frames, nil
}

func (a *app) runStopAction(w http.ResponseWriter, r *http.Request, attempt Attempt) {
	outcome, err := a.workflow.StopRunTurn(r.Context(), attempt, r.FormValue("turn_id"))
	if err != nil {
		message, status := "Outcome unknown; this request may have executed or been interrupted. Check the conversation before sending again.", http.StatusServiceUnavailable
		switch {
		case errors.Is(err, errStopUnavailable):
			message, status = "Agent unavailable. No request sent.", http.StatusConflict
		case errors.Is(err, errStopBusy):
			message, status = "Run busy. No request sent.", http.StatusConflict
		case errors.Is(err, errStopSnapshot):
			message = "Cannot read the agent state. No request sent."
		case errors.Is(err, errStopIdentityChanged):
			message, status = "Run identity changed. No request sent.", http.StatusConflict
		case errors.Is(err, errStopTurnChanged):
			message, status = "Active turn changed or cannot be stopped. No request sent.", http.StatusConflict
		case errors.Is(err, errStopTargetChanged):
			message, status = "Cancellation target changed.", http.StatusConflict
		case errors.Is(err, errStopAuditFailed):
			message = "Cannot record cancellation. No request sent."
		}
		a.responseError(w, r, message, status)
		return
	}
	fresh := outcome.Attempt
	notice := "Cancellation requested; wait for agent confirmation."
	if !datastarRequest(r) {
		http.Redirect(w, r, detailPath(fresh, detailTab(r)), http.StatusSeeOther)
		return
	}
	var out bytes.Buffer
	_ = notification(notice, "").Render(r.Context(), &out)
	stream := newSSEWriter(w)
	if stream.write(elementsFrame(out.String())) != nil {
		return
	}

}

func validAttemptID(id string) bool { _, err := uuid.Parse(id); return err == nil }

func validRunDestination(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func (a *app) loadChatDetail(ctx context.Context, d *runDetail) {
	d.MessageID = uuid.NewString()
	d.Chat = a.workflow.chatMessages(d.Record)
	for i := range d.Chat {
		var receipt reportprotocol.ChatReceipt
		if json.Unmarshal([]byte(d.Chat[i].Receipt), &receipt) == nil && receipt.UserIndex != nil && d.Observation.Snapshot != nil {
			index := *receipt.UserIndex
			if index >= 0 && index < len(d.Messages) {
				for _, part := range d.Messages[index].Parts {
					if part.Text == d.Chat[i].Text {
						d.Chat[i].InTranscript = true
						if d.SentAt == nil {
							d.SentAt = make(map[int]time.Time)
						}
						d.SentAt[index] = time.UnixMilli(d.Chat[i].CreatedAt)
					}
				}
			}
		}
	}
	tx, err := a.s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	token, _, err := chatInteraction(tx, d.Record)
	if err == nil {
		d.Interaction = token
	}
	class, err := classifyChat(tx, d.Record)
	if err == nil && class == "followup" {
		d.Policy.Mode = "queue"
	}
}
func (a *app) runChatAction(w http.ResponseWriter, r *http.Request, record RunRecord) {
	_, err := a.workflow.QueueRunChat(r.Context(), record.Mode, record.ID, r.FormValue("message_id"), r.FormValue("text"), r.FormValue("interaction"))
	if err != nil {
		a.responseError(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	if !datastarRequest(r) {
		http.Redirect(w, r, record.path(detailTab(r)), http.StatusSeeOther)
		return
	}
	var out bytes.Buffer
	_ = notification("Message queued.", "").Render(r.Context(), &out)
	stream := newSSEWriter(w)
	if stream.write(elementsFrame(out.String())) != nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"runConfirmedDraft": r.FormValue("text"), "runAcceptedMessage": r.FormValue("message_id")})
	_ = stream.write("event: datastar-patch-signals\ndata: signals " + string(payload) + "\n\n")
}

// Reconciliation fences the original command; it never replays uncertain work
// or creates another command. New text is a separate explicit submission.
func (a *app) reconcileRunChatAction(w http.ResponseWriter, r *http.Request, record RunRecord) {
	err := a.workflow.ReconcileRunChat(r.Context(), record, r.FormValue("chat_id"))
	if err != nil {
		message, status := "Unable to save reconciliation; retry the same action.", http.StatusServiceUnavailable
		switch {
		case errors.Is(err, ErrRunChatChanged):
			message, status = "Message changed; reload before reconciling.", http.StatusConflict
		case errors.Is(err, errChatCommandUnavailable):
			message, status = "Command unavailable.", http.StatusConflict
		case errors.Is(err, errChatReconcileUnconfirmed):
			message, status = "Owner has not confirmed safe reconciliation; no new execution started.", http.StatusConflict
		}
		a.responseError(w, r, message, status)
		return
	}
	http.Redirect(w, r, record.path("chat"), http.StatusSeeOther)
}
