package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	d := runDetail{Record: buildRun(attempt), Tab: detailTab(r), Observation: obs}
	d.Policy = a.workflow.runPolicy(attempt, obs.Snapshot, online && !obs.Unreachable)
	if obs.Snapshot != nil {
		d.Messages = projectRunMessages(obs.Snapshot.Messages)
	}
	a.loadChatDetail(&d)
	return d
}

func (a *app) historyDetailModel(r *http.Request, record RunRecord, obs runObservation, online bool) runDetail {
	if record.Mode != "Planning" {
		return a.detailModel(r, record.Build, obs, online)
	}
	d := runDetail{Record: record, Tab: detailTab(r), Observation: obs,
		Policy: runPolicy{Send: true, Mode: "steer", Reason: "Messages are saved before delivery. Specification approval still requires the card action."}}
	if obs.Snapshot != nil {
		d.Messages = projectRunMessages(obs.Snapshot.Messages)
	}
	a.loadChatDetail(&d)
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
		a.runDetailAction(w, r, attempt.Build, action)
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
	previous := ""
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
		detail := a.historyDetailModel(r, fresh, obs, available)
		var out bytes.Buffer
		if runDetailContent(detail).Render(r.Context(), &out) != nil || runDetailControls(detail, CSRFToken(r)).Render(r.Context(), &out) != nil {
			return
		}
		if out.String() != previous {
			if stream.write(elementsFrame(out.String())) != nil {
				return
			}
			previous = out.String()
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
func (a *app) runDetailAction(w http.ResponseWriter, r *http.Request, attempt Attempt, action string) {
	online := a.workflow.apiAvailable(r.Context(), attempt)
	if !online {
		a.responseError(w, r, "Agent unavailable. No request sent.", http.StatusConflict)
		return
	}
	release, ok := a.workflow.runAPI.reserveSend(attempt)
	if !ok {
		a.responseError(w, r, "Run busy. No request sent.", http.StatusConflict)
		return
	}
	defer release()
	snap, err := a.workflow.runAPI.snapshot(r.Context(), attempt)
	if err != nil {
		a.responseError(w, r, "Cannot read the agent state. No request sent.", http.StatusServiceUnavailable)
		return
	}
	fresh, err := a.workflow.attemptByID(attempt.ID)
	if err != nil || fresh.AgentID != attempt.AgentID || fresh.RunID != attempt.RunID || fresh.RemoteAttemptID != attempt.RemoteAttemptID {
		a.responseError(w, r, "Run identity changed. No request sent.", http.StatusConflict)
		return
	}
	policy := a.workflow.runPolicy(fresh, snap, true)
	notice := "Cancellation requested; wait for agent confirmation."
	turn := r.FormValue("turn_id")
	if !policy.Stop || turn == "" || turn != snap.TurnID {
		a.responseError(w, r, "Active turn changed or cannot be stopped. No request sent.", http.StatusConflict)
		return
	}
	activeWorkflow := !fresh.Ready && policy.Mode == "steer"
	tx, e := a.s.writeTx()
	if e == nil {
		class, ce := classifyChat(tx, buildRun(fresh))
		chat, he := chatContinuation(tx, fresh.TaskID)
		_ = tx.Rollback()
		if ce == nil && he == nil && (class == "followup" || chat) {
			activeWorkflow = false
		}
	}
	if activeWorkflow {
		if err = a.workflow.checkCancelLatest(fresh); err != nil {
			a.responseError(w, r, "Cancellation target changed.", http.StatusConflict)
			return
		}
		if err = a.workflow.cancelAudit(fresh, turn, "pending", ""); err != nil {
			a.responseError(w, r, "Cannot record cancellation. No request sent.", http.StatusServiceUnavailable)
			return
		}
	}
	result, callErr := a.workflow.runAPI.cancelTurn(r.Context(), fresh, turn)
	err = callErr
	if err == nil && (result.RunID != fresh.RunID || result.TurnID != turn || result.Status != runtime.CancelRequested) {
		err = errors.New("cancellation not confirmed")
	}
	if activeWorkflow {
		status := "requested"
		if err != nil {
			status = "uncertain"
		}
		auditErr := a.workflow.cancelAudit(fresh, turn, status, "")
		if err == nil {
			err = auditErr
		}
	}
	if err != nil {
		a.responseError(w, r, "Outcome unknown; this request may have executed or been interrupted. Check the conversation before sending again.", http.StatusServiceUnavailable)
		return
	}
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

func (a *app) loadChatDetail(d *runDetail) {
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
					}
				}
			}
		}
	}
	tx, err := a.s.writeTx()
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
	m, err := scanChat(a.s.db.QueryRow(`SELECT `+chatColumns+` FROM run_chat WHERE id=? AND mode=? AND local_id=? AND status='ambiguous'`, r.FormValue("chat_id"), record.Mode, record.ID))
	if err != nil {
		a.responseError(w, r, "Message changed; reload before reconciling.", http.StatusConflict)
		return
	}
	var cmd reportprotocol.Chat
	if json.Unmarshal([]byte(m.Request), &cmd) != nil {
		a.responseError(w, r, "Command unavailable.", http.StatusConflict)
		return
	}
	cmd.Reconcile = true
	cmd.Token = runapitoken.Sign(a.workflow.runAPI.key, cmd.Target.AgentID, time.Now())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	receipt, err := reportprotocol.SendChat(ctx, a.workflow.runAPI.conn, cmd)
	if err != nil || receipt.Status != "failed" || receipt.Failure != "reconciled_not_replayed" {
		a.responseError(w, r, "Owner has not confirmed safe reconciliation; no new execution started.", http.StatusConflict)
		return
	}
	raw, _ := json.Marshal(receipt)
	_, err = a.s.db.Exec(`UPDATE run_chat SET status='failed',failure=?,receipt_json=?,updated_at=? WHERE id=? AND status='ambiguous'`, receipt.Failure, string(raw), time.Now().UnixMilli(), m.ID)
	if err != nil {
		a.responseError(w, r, "Unable to save reconciliation; retry the same action.", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, record.path("chat"), http.StatusSeeOther)
}
