package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/runapitoken"
	"net/http"
	"strings"
	"time"
)

type runDetail struct {
	Attempt     Attempt
	Tab         string
	Observation runObservation
	Messages    []runMessage
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
func detailPath(a Attempt, tab string) string { return "/runs/" + a.ID + "?tab=" + tab }
func (w *Workflow) attemptByID(id string) (Attempt, error) {
	return scanAttempt(w.s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE id=?`, id))
}
func (w *Workflow) apiAvailable(ctx context.Context, a Attempt) bool {
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

// Eligibility is re-read for every action. Run-completed without accepted
// workflow completion never enables a manual chat resume.
func (w *Workflow) runPolicy(a Attempt, snap *bonnieclient.Snapshot, online bool) runPolicy {
	p := runPolicy{Reason: "Read-only. Use the card's authoritative Resume/Retry or guidance action."}
	if !online || snap == nil {
		p.Reason = "Agent offline, unreachable, or without run API support. Stored workflow data only."
		return p
	}
	if snap.RunID != a.RunID || snap.Suspend != nil || snap.State == runtime.RunWaiting || snap.State == runtime.RunRetired || snap.State == runtime.RunCancelled || snap.State == runtime.RunFailed || snap.State == runtime.RunInterrupted {
		return p
	}
	latest, err := w.CardResult(a.CardID)
	if err != nil {
		return p
	}
	var lane string
	if err := w.s.db.QueryRow(`SELECT status FROM cards WHERE id=?`, a.CardID).Scan(&lane); err != nil {
		return p
	}
	if lane != "Building" && lane != "Fix" && lane != "Done" {
		return p
	}
	var pending, completed bool
	err = w.s.db.QueryRow(`SELECT
 EXISTS(SELECT 1 FROM failed_resumes WHERE card_id=? AND status='pending') OR
 EXISTS(SELECT 1 FROM workflow_cancel_requests WHERE task_id=? AND status IN ('pending','requested','uncertain')) OR
 EXISTS(SELECT 1 FROM workflow_interactions WHERE task_id=? AND status IN ('pending','queued')),
 EXISTS(SELECT 1 FROM typed_reports WHERE task_id=? AND kind='complete_task')`, a.CardID, a.TaskID, a.TaskID, a.TaskID).Scan(&pending, &completed)
	if err != nil || pending {
		return p
	}
	if latest.ID != a.ID {
		return p
	}
	if a.Ready || completed {
		p.Send = true
		p.Mode = "queue"
		p.Reason = "Follow-up only; workflow completion and readiness remain unchanged."
	} else if a.RunState == "running" && snap.State == runtime.RunRunning && attemptActive(a) && (lane == "Building" || lane == "Fix") {
		p.Send = true
		p.Mode = "steer"
		p.Reason = "Message the active turn."
	}
	if p.Send && snap.State == runtime.RunRunning && snap.TurnID != "" {
		p.Stop = true
		p.Turn = snap.TurnID
	}
	return p
}
func (a *app) detailModel(r *http.Request, attempt Attempt, obs runObservation, online bool) runDetail {
	d := runDetail{Attempt: attempt, Tab: detailTab(r), Observation: obs}
	d.Policy = a.workflow.runPolicy(attempt, obs.Snapshot, online && !obs.Unreachable)
	if obs.Snapshot != nil {
		d.Messages = projectRunMessages(obs.Snapshot.Messages)
	}
	return d
}

// Exact dynamic routes pass the same authentication, body and setup guards as
// the fixed screens. Browser input can select an attempt, never a run/subject.
func (a *app) runDetailRoute(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/runs/"), "/")
	if len(parts) > 2 || len(parts) == 0 || !validAttemptID(parts[0]) || a.workflow == nil {
		http.NotFound(w, r)
		return
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
		if action != "events" && action != "chat" && action != "stop" {
			http.NotFound(w, r)
			return
		}
	}
	method := http.MethodGet
	if action == "chat" || action == "stop" {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	attempt, err := a.workflow.attemptByID(parts[0])
	if err != nil || attempt.AgentID == "" || attempt.RunID == "" || attempt.RemoteAttemptID == "" {
		http.NotFound(w, r)
		return
	}
	if _, err = runapitoken.Subject("bonnie", attempt.AgentID); err != nil || !validRunDestination(attempt.RunID) {
		http.NotFound(w, r)
		return
	}
	if method == http.MethodPost {
		if !a.csrf(w, r) {
			return
		}
		ready, e := a.s.NATSConfigured()
		if e != nil || !ready {
			a.responseError(w, r, "NATS setup required.", http.StatusConflict)
			return
		}
		a.runDetailAction(w, r, attempt, action)
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
	detail := a.detailModel(r, attempt, obs, online)
	data.detail = &detail
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = appPage(data).Render(r.Context(), w)
}
func (a *app) runDetailEvents(w http.ResponseWriter, r *http.Request, attempt Attempt, online bool) {
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
		fresh, e := a.workflow.attemptByID(attempt.ID)
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
		detail := a.detailModel(r, fresh, obs, available)
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
	notice := "Message confirmed."
	if action == "chat" {
		text := r.FormValue("text")
		if !policy.Send {
			a.responseError(w, r, policy.Reason, http.StatusConflict)
			return
		}
		if strings.TrimSpace(text) == "" || len(text) > 64<<10 {
			a.responseError(w, r, "Enter a message of at most 64 KiB.", http.StatusBadRequest)
			return
		}
		err = a.workflow.runAPI.send(r.Context(), fresh, text, policy.Mode)
	} else {
		turn := r.FormValue("turn_id")
		if !policy.Stop || turn == "" || turn != snap.TurnID {
			a.responseError(w, r, "Active turn changed or cannot be stopped. No request sent.", http.StatusConflict)
			return
		}
		activeWorkflow := !fresh.Ready && policy.Mode == "steer"
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
		notice = "Cancellation requested; wait for agent confirmation."
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
	if action == "chat" {
		payload, _ := json.Marshal(map[string]string{"runConfirmedDraft": r.FormValue("text")})
		_ = stream.write("event: datastar-patch-signals\ndata: signals " + string(payload) + "\n\n")
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
