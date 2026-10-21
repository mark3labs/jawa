package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

// mutation is the single registry for mutation routing and admission policy.
// Preflight preserves validations that historically preceded workflow availability.
type mutation struct {
	needsWorkflow   bool
	allowBeforeNATS bool
	preflight       func(*app, http.ResponseWriter, *http.Request, *mutationContext) error
	run             func(*app, http.ResponseWriter, *http.Request, *mutationContext) error
}

type mutationContext struct {
	effects  mutationEffects
	ready    bool
	handled  bool // The handler already wrote a custom response.
	position int
	operator string
}

var mutations = map[string]mutation{
	"/settings/apikeys":            {allowBeforeNATS: true, run: (*app).mutateAPIKeyCreate},
	"/settings/apikeys/revoke":     {allowBeforeNATS: true, run: (*app).mutateAPIKeyRevoke},
	"/cards/auto-merge":            {needsWorkflow: true, run: (*app).mutateCardAutoMerge},
	"/cards/assign":                {needsWorkflow: true, run: (*app).mutateAssignment},
	"/prs/refresh":                 {run: (*app).mutatePRsRefresh},
	"/logout":                      {allowBeforeNATS: true, run: (*app).mutateLogout},
	"/projects":                    {run: (*app).mutateProjects},
	"/cards":                       {run: (*app).mutateCards},
	"/cards/move":                  {needsWorkflow: true, run: (*app).mutateCardsMove, preflight: (*app).mutateCardsMovePreflight},
	"/cards/plan":                  {needsWorkflow: true, run: (*app).mutateCardsPlan},
	"/cards/planning/reply":        {needsWorkflow: true, run: (*app).mutatePlanningReply},
	"/cards/planning/retry":        {needsWorkflow: true, run: (*app).mutatePlanningRetry},
	"/cards/planning/resume":       {needsWorkflow: true, run: (*app).mutatePlanningResume},
	"/cards/planning/approve":      {needsWorkflow: true, run: (*app).mutatePlanningApprove},
	"/cards/planning/cancel":       {needsWorkflow: true, run: (*app).mutatePlanningCancel},
	"/cards/resume":                {needsWorkflow: true, run: (*app).mutateCardsResume},
	"/cards/merge":                 {needsWorkflow: true, preflight: (*app).mutateCardsForceReadyPreflight, run: (*app).mutateCardsMerge},
	"/cards/mark-merged":           {needsWorkflow: true, run: (*app).mutateCardsMarkMerged},
	"/cards/fix":                   {needsWorkflow: true, run: (*app).mutateCardsFix},
	"/cards/guide":                 {needsWorkflow: true, run: (*app).mutateCardsGuide},
	"/cards/retry":                 {needsWorkflow: true, run: (*app).mutateCardsRetry},
	"/cards/reset":                 {needsWorkflow: true, run: (*app).mutateCardsReset},
	"/cards/delete":                {needsWorkflow: true, run: (*app).mutateCardsReset},
	"/cards/archive":               {run: (*app).mutateCardsArchive},
	"/cards/restore":               {run: (*app).mutateCardsArchive},
	"/cards/cancel":                {needsWorkflow: true, run: (*app).mutateCardsCancel},
	"/cards/force-ready":           {needsWorkflow: true, run: (*app).mutateCardsForceReady, preflight: (*app).mutateCardsForceReadyPreflight},
	"/cards/force-stop":            {needsWorkflow: true, run: (*app).mutateCardsForceStop},
	"/settings/orchestrator/reset": {run: (*app).mutateSettingsOrchestratorReset},
	"/settings/orchestrator":       {run: (*app).mutateSettingsOrchestrator},
	"/settings/nats":               {allowBeforeNATS: true, run: (*app).mutateSettingsNATS},
}

func (a *app) mutatePRsRefresh(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	a.refreshPRChecks(w, r)
	ctx.handled = true
	return nil
}

func (a *app) mutateLogout(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	a.clear(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
	ctx.handled = true
	return nil
}

func (a *app) mutateProjects(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	var project Project
	provider := strings.ToLower(strings.TrimSpace(r.FormValue("provider")))
	repo := r.FormValue("repo")
	if r.Form.Has("repo_name") {
		repo, err = projectRepositoryURL(provider, r.FormValue("repo_name"), r.FormValue("forgejo_url"))
		if err != nil {
			return err
		}
	}
	project, err = a.s.CreateProjectDetails(r.FormValue("name"), provider, repo, r.FormValue("base_branch"))
	if err == nil {
		ctx.effects.goTo = boardPath(project.ID)
	}

	return err
}

func (a *app) mutateCards(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	mode, err := submittedAutoMergeMode(r.FormValue("auto_merge_mode"))
	if err != nil {
		return err
	}
	if r.FormValue("create_issue") != "" || r.FormValue("issue_recovery") != "" || r.FormValue("issue_submission") != "" && a.hasIssueSubmission(r.FormValue("issue_submission")) {
		err = a.submitIssueCard(r)
		if err != nil {
			a.issueCardError(w, r, err)
			ctx.handled = true
			return nil
		}
		return err
	}
	_, err = a.s.CreateCardOptions(r.FormValue("project_id"), r.FormValue("title"), r.FormValue("description"), r.FormValue("issue_url"), mode)

	return err
}

func (a *app) mutateCardsMovePreflight(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	raw := r.FormValue("position")
	ctx.position = 0
	if raw != "" {
		for _, c := range raw {
			if c < '0' || c > '9' {
				a.responseError(w, r, "Invalid card position. Refresh the board and try again.", http.StatusBadRequest)
				ctx.handled = true
				return nil
			}
		}
		ctx.position, err = strconv.Atoi(raw)
		if err != nil {
			a.responseError(w, r, "Invalid card position. Refresh the board and try again.", http.StatusBadRequest)
			ctx.handled = true
			return nil
		}
	}

	return err
}

func (a *app) mutateCardsMove(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	cardID, status := r.FormValue("card_id"), r.FormValue("status")
	if status == "Building" {
		card, loadErr := a.s.cardByID(r.Context(), cardID)
		if loadErr != nil {
			err = loadErr
			return err
		}
		if card.Status == "Planning" {
			err = errors.New("review and approve a plan revision before building")
			return err
		}
	}
	err = a.workflow.MoveCard(cardID, status, ctx.position)

	return err
}

func (a *app) mutateCardsPlan(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.MoveCard(r.FormValue("card_id"), "Planning", 0)
}

func (a *app) mutatePlanningReply(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.ReplyPlanning(r.FormValue("card_id"), r.FormValue("reply"), r.FormValue("expected_interaction"))
}

func (a *app) mutatePlanningRetry(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.RetryPlanning(r.FormValue("card_id"))
}

func (a *app) mutatePlanningCancel(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.CancelPlanning(r.FormValue("card_id"))
}

func (a *app) mutatePlanningResume(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.resumeMutation(r, true)
}

func (a *app) mutatePlanningApprove(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	// Never substitute the latest revision: approval is a user-visible CAS.
	revision, err := strconv.Atoi(r.FormValue("expected_revision"))
	if err != nil || revision < 1 {
		return errors.New("select a valid plan revision to approve")
	}
	return a.workflow.ApprovePlanning(r.FormValue("card_id"), revision)
}

func (a *app) mutateCardsResume(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.resumeMutation(r, false)
}

func (a *app) resumeMutation(r *http.Request, planning bool) error {
	if r.FormValue("expected_resume") == "" {
		return errors.New("refresh before resuming")
	}
	return a.workflow.resumeFailed(r.FormValue("card_id"), planning, r.FormValue("expected_resume"))
}

func (a *app) mutateCardsMarkMerged(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.MarkPRMerged(r.Context(), r.FormValue("card_id"), r.FormValue("expected_attempt_id"), r.FormValue("expected_pr_url"))
}

func (a *app) mutateCardsFix(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.FixCard(r.FormValue("card_id"), r.FormValue("message"))
}

func (a *app) mutateCardsGuide(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.GuideBlocked(r.FormValue("card_id"), r.FormValue("message"), r.FormValue("expected_interaction"))
}

func (a *app) mutateCardsRetry(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.Retry(r.FormValue("card_id"))
}

func (a *app) mutateCardsReset(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	if r.URL.Path == "/cards/reset" {
		err = a.workflow.ResetCard(r.FormValue("card_id"))
	} else if r.FormValue("acknowledge_orphan") == "yes" {
		err = a.workflow.DeleteUnreconciledCard(r.FormValue("card_id"))
	} else {
		err = a.workflow.DeleteCard(r.FormValue("card_id"))
	}

	return err
}

func (a *app) mutateCardsArchive(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	return a.workflow.SetArchived(r.Context(), r.FormValue("card_id"), r.URL.Path == "/cards/archive")
}

func (a *app) mutateCardsCancel(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.workflow.CancelCard(r.Context(), r.FormValue("card_id"))
	if err == nil {
		ctx.effects.flash = "Cancellation was requested; it is not confirmed until the agent reports cancellation. External actions already taken cannot be undone."
	}

	return err
}

func (a *app) mutateCardsForceReadyPreflight(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	ctx.operator, err = a.requestOperator(r)

	return err
}

func (a *app) mutateCardsForceReady(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.workflow.ForceReady(r.Context(), r.FormValue("card_id"), r.FormValue("expected"), r.FormValue("pr_number"), r.FormValue("reason"), ctx.operator, r.FormValue("acknowledge") == "yes")
	if err == nil {
		ctx.effects.flash = "Card moved to Done. Readiness checks were bypassed; the PR was not merged."
	}

	return err
}

func (a *app) mutateCardsForceStop(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	var err error
	if r.FormValue("acknowledge") != "yes" {
		err = errors.New("confirm that the agent may still be running before force stopping")
	} else {
		err = a.workflow.ForceStop(r.FormValue("card_id"))
		if err == nil {
			ctx.effects.flash = "Run force-stopped. Later reports from it are ignored; the agent was asked to stop but may still be running."
		}
	}

	return err
}

func (a *app) mutateSettingsOrchestratorReset(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.s.ClearDashboardOverrides()
	if err == nil {
		ctx.effects.flash = "Dashboard overrides cleared. Restart required for listener changes."
		if a.logger != nil {
			if a.debugDefault {
				a.logger.SetLevel(log.DebugLevel)
			} else {
				a.logger.SetLevel(log.InfoLevel)
			}
		}
	}

	return err
}

func (a *app) mutateSettingsOrchestrator(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	values, err := a.s.DashboardOverrides()
	if err != nil {
		return err
	}
	for _, key := range []string{"listen", "nats-listen", "mcp-listen"} {
		if value := strings.TrimSpace(r.FormValue(key)); value != "" {
			if e := validatedListenAddress(value); e != nil {
				err = fmt.Errorf("invalid %s address", key)
				break
			}
			values[key] = value
		}
	}
	if err == nil && r.Form.Has("debug") {
		value := r.Form.Get("debug")
		if value != "true" && value != "false" {
			err = errors.New("debug must be true or false")
		} else {
			values["debug"] = value
		}
	}
	for _, pair := range [][2]string{{"github-token", "github-token"}, {"forgejo-token", "forgejo-token"}} {
		if r.Form.Has(pair[0]+"-clear") && r.Form.Get(pair[0]+"-clear") == "true" {
			delete(values, pair[1])
		}
		if value := strings.TrimSpace(r.FormValue(pair[0])); value != "" {
			values[pair[1]] = value
		}
	}
	if err == nil && r.Form.Has("auto-merge") {
		value := r.Form.Get("auto-merge")
		if value != "true" && value != "false" {
			err = errors.New("auto-merge must be true or false")
		} else {
			values["auto-merge"] = value
		}
	}
	if err == nil && r.Form.Has("auto-resume-failed-runs") {
		value := r.Form.Get("auto-resume-failed-runs")
		if value != "true" && value != "false" {
			err = errors.New("auto-resume-failed-runs must be true or false")
		} else {
			values["auto-resume-failed-runs"] = value
		}
	}
	if err == nil && r.Form.Has("allow-no-ci") {
		value := r.Form.Get("allow-no-ci")
		if value != "true" && value != "false" {
			err = errors.New("allow-no-ci must be true or false")
		} else {
			values["allow-no-ci"] = value
		}
	}
	if err == nil {
		err = a.s.ReplaceDashboardOverrides(values)
		ctx.effects.flash = "Overrides saved. Listener changes require a restart; debug logging applies immediately."
	}
	if err == nil && values["debug"] != "" && a.logger != nil {
		if values["debug"] == "true" {
			a.logger.SetLevel(log.DebugLevel)
		} else {
			a.logger.SetLevel(log.InfoLevel)
		}
	}

	return err
}

func (a *app) mutateSettingsNATS(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.rotateNATS(r.FormValue("url"), r.FormValue("username"), r.FormValue("password"))
	if err == nil {
		ctx.effects.flash = "Agent credentials rotated. Agents using the old password were disconnected."
		if !ctx.ready {
			ctx.effects.goTo = "/board"
		}
	}
	return err
}

// serveMutation retains the shared admission and response order for all mutations.
func (a *app) serveMutation(w http.ResponseWriter, r *http.Request, route mutation, ready bool) {
	var err error
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if !a.csrf(w, r) {
		return
	}
	if !ready && !route.allowBeforeNATS {
		a.responseError(w, r, "NATS authentication setup is required. Open Settings and set the agent username and password before creating projects, cards or running work.", http.StatusConflict)
		return
	}
	ctx := mutationContext{ready: ready}
	if route.preflight != nil {
		err = route.preflight(a, w, r, &ctx)
	}
	if ctx.handled {
		return
	}
	if err == nil && route.needsWorkflow && a.workflow == nil {
		message := "workflow unavailable"
		if r.URL.Path == "/cards/move" {
			message = "workflow is unavailable"
		}
		err = errors.New(message)
	}
	if err == nil {
		err = route.run(a, w, r, &ctx)
	}
	if ctx.handled {
		return
	}
	fx := ctx.effects

	if err != nil {
		a.responseError(w, r, "Unable to apply request: "+strings.TrimRight(err.Error(), ". \t\n")+". Refresh the board and try again.", http.StatusBadRequest)
		return
	}
	if datastarRequest(r) {
		a.snapshot(w, r, true, fx)
		return
	}
	// Native form fallback: return to the screen the form came from.
	back := parseViewParams(r, "")
	if fx.goTo != "" {
		http.Redirect(w, r, fx.goTo, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back.Path(), http.StatusSeeOther)
}

func (a *app) mutateCardAutoMerge(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.workflow.SetCardAutoMerge(r.FormValue("card_id"), r.FormValue("expected_mode"), r.FormValue("auto_merge_mode"))
	if err == nil {
		ctx.effects.goTo = parseViewParams(r, "").Path()
	}
	return err
}

func (a *app) mutateCardsMerge(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	bounded, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	err := a.workflow.MergePR(bounded, r.FormValue("card_id"), r.FormValue("operation"), r.FormValue("snapshot"), r.FormValue("method"), ctx.operator)
	if err != nil {
		message := errMergeRejected.Error()
		if errors.Is(err, errMergeLocked) {
			message = errMergeLocked.Error()
		}
		a.responseError(w, r, message, http.StatusConflict)
		ctx.handled = true
	}
	return nil
}
