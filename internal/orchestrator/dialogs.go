package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/axadrn/shadcn-templ/v2/components/dialog"
)

var dialogNames = map[string]bool{"project": true, "card": true, "delete": true, "merge": true, "fix": true, "guide": true, "cancel": true, "ready": true, "force": true, "assign": true}

func dialogOpen(name, cardID, extra string) string {
	q := url.Values{}
	if cardID != "" {
		q.Set("card_id", cardID)
	}
	if extra != "" {
		q.Set("x", extra)
	}
	return "@get(" + strconv.Quote("/dialogs/"+name+"?"+q.Encode()) + ", {filterSignals: {include: /^$/}})"
}

func dialogForm(name, route string) templ.Attributes {
	return templ.Attributes{
		"data-indicator:_" + name + "-pending": true,
		"data-class:pending":                   "$_" + name + "Pending",
		"data-on:submit__prevent":              fmt.Sprintf("!$_%sPending && @post('%s', {contentType:'form', retry:'never', requestCancellation:'cleanup'})", name, route),
	}
}

// button.Button supplies type, so do not duplicate the component's type attribute.
func dialogClose(ctx context.Context) templ.Attributes {
	attrs := dialog.Close(ctx)
	delete(attrs, "type")
	return attrs
}

func (a *app) dialogBody(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/dialogs/")
	if r.Method != http.MethodGet || !datastarRequest(r) {
		http.NotFound(w, r)
		return
	}
	switch name {
	case "card/issues":
		a.cardIssues(w, r)
		return
	case "assign/agents":
		a.assignmentAgents(w, r)
		return
	}
	if !dialogNames[name] {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	d, err := loadPageData(ctx, a.s, a.workflow, CSRFToken(r), parseViewParams(r, ""), a.runtimeDefaults)
	if err != nil {
		a.responseError(w, r, "Unable to open this dialog. Please try again.", http.StatusServiceUnavailable)
		return
	}
	component, err := a.dialogComponent(ctx, name, newOperationID(), d, r.URL.Query())
	if err != nil {
		a.responseError(w, r, err.Error(), http.StatusConflict)
		return
	}
	var buf bytes.Buffer
	if component.Render(ctx, &buf) != nil {
		return
	}
	_ = newSSEWriter(w).write(elementsFrameMode("inner", "#dialog-slot", buf.String()))
}

func (a *app) dialogComponent(ctx context.Context, name, op string, d pageData, q url.Values) (templ.Component, error) {
	switch name {
	case "project":
		return projectDialog(d, op), nil
	case "card":
		return cardDialog(d, op), nil
	}
	var c Card
	found := false
	for _, card := range d.cards {
		if card.ID == q.Get("card_id") {
			c = card
			found = true
			break
		}
	}
	refused := errors.New("this action is no longer available; refresh the workspace and try again")
	if !found {
		return nil, refused
	}
	p, _ := selectedProject(d.projects, c.ProjectID)
	switch name {
	case "delete":
		if canAction(c, d.attempts, "delete") {
			return deleteDialog(d, op, c, cardNeedsReconciliation(c, d.attempts)), nil
		}
	case "fix":
		if canAction(c, d.attempts, "fix") {
			message := ""
			if q.Get("x") == "conflict" {
				message = conflictFixMessage
			}
			return fixDialog(d, op, c, message), nil
		}
	case "guide":
		if d.canGuide(c) {
			b, _ := d.blocker(c)
			return guideDialog(d, op, c, b.Token), nil
		}
	case "cancel":
		if canAction(c, d.attempts, "cancel") {
			return cancelDialog(d, op, c), nil
		}
	case "force":
		if canAction(c, d.attempts, "force-stop") {
			return forceDialog(d, op, c), nil
		}
	case "ready":
		if d.canForceReady(c) {
			attempt := latestAttempt(c.ID, d.attempts)[0]
			pr, _ := forceReadyPR(c, attempt, p, "")
			return readyDialog(d, op, c, forceReadyToken(c, attempt, p), pr), nil
		}
	case "merge":
		if d.canMerge(c) {
			attempt := latestAttempt(c.ID, d.attempts)[0]
			check := d.prChecks[c.ID]
			return mergeDialog(d, op, c, mergeSnapshot(c, attempt, p, check.HeadSHA, check.BaseSHA), attempt.PRURL, p.BaseBranch, check.HeadSHA), nil
		}
	case "assign":
		mode := q.Get("x")
		if mode == "plan" && c.Status == "Todo" || mode == "build" && (c.Status == "Todo" || c.Status == "Planning" && d.planning[c.ID].CanApprove) || mode == "fix" && canAction(c, d.attempts, "fix") {
			choices, err := a.workflow.choices(ctx)
			return assignDialog(d, op, c, mode, choices, err), nil
		}
	}
	return nil, refused
}
