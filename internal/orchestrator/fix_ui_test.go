package orchestrator

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestFixEndpointAndSnapshot(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	original := repairDone(t, a.workflow, c)
	_, err := a.s.db.Exec(`INSERT INTO workflow_pr_checks(attempt_id,pr_url,provider,repo,base_branch,state,checked_at)
 SELECT ?,?,provider,repo,base_branch,'conflict',1 FROM projects WHERE id=?`, original.ID, original.PRURL, c.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	body := request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String()
	for _, want := range []string{`data-status="Fix"`, "Pull request has merge conflicts.", "Fix Merge Conflict", "Send to Fix…", conflictFixMessage, `id="fix-dialog"`, `name="message"`, `required`, `data-can-fix="true"`} {
		if !strings.Contains(body, want) {
			t.Errorf("board missing %q", want)
		}
	}
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "message": {"Repair review feedback"}, "view": {"board"}, "project": {c.ProjectID}}
	status(t, request(a, "GET", "/cards/fix", nil, session, csrf), http.StatusMethodNotAllowed)
	status(t, request(a, "POST", "/cards/fix", nil, session, csrf), http.StatusForbidden)
	form.Set("message", " \n ")
	status(t, request(a, "POST", "/cards/fix", form, session, csrf), http.StatusBadRequest)
	failed := assertSSE(t, sseRequest(a, "POST", "/cards/fix", form, session, csrf))
	if strings.Contains(failed, `"fixOpen":false`) {
		t.Fatal("failure closed the draft dialog")
	}
	assertActionLane(t, a.workflow, c.ProjectID, "Done", c.ID)
	form.Set("message", "Repair review feedback")
	body = assertSSE(t, sseRequest(a, "POST", "/cards/fix", form, session, csrf))
	for _, want := range []string{`"fixOpen":false`, `"fixMessage":""`, `"fixCard":""`} {
		if !strings.Contains(body, want) {
			t.Errorf("success missing %s", want)
		}
	}
	if strings.Contains(body, "Pull request has merge conflicts.") {
		t.Fatal("stale conflict visible during repair")
	}
	assertActionLane(t, a.workflow, c.ProjectID, "Fix", c.ID)
	repair, err := a.workflow.CardResult(c.ID)
	if err != nil || repair.RepairMessage != "Repair review feedback" {
		t.Fatalf("repair %+v: %v", repair, err)
	}
	status(t, request(a, "POST", "/cards/fix", form, session, csrf), http.StatusBadRequest)
	latest, _ := a.workflow.CardResult(c.ID)
	if latest.Number != repair.Number {
		t.Fatal("duplicate repair queued")
	}
	if _, err = a.s.db.Exec(`UPDATE workflow_attempts SET state='failed',run_state='failed',outcome_json='{}',error='conflict' WHERE id=?`, repair.ID); err != nil {
		t.Fatal(err)
	}
	body = request(a, "GET", boardPath(c.ProjectID), nil, session, csrf).Body.String()
	if !strings.Contains(body, "Retry attempt") {
		t.Fatal("Fix retry missing")
	}
	status(t, request(a, "POST", "/cards/retry", form, session, csrf), http.StatusSeeOther)
	latest, _ = a.workflow.CardResult(c.ID)
	if latest.Number != repair.Number+1 || latest.RepairMessage != repair.RepairMessage {
		t.Fatal("retry lost repair assignment")
	}
}

func TestConflictWarningOnlyForConflict(t *testing.T) {
	c := Card{ID: "done", Status: "Done"}
	attempts := []Attempt{{CardID: c.ID, Number: 1, State: "ready", Ready: true, PRURL: "https://github.com/example/repo/pull/18"}}
	for _, state := range []string{"checking", "conflict", "clear", "unknown", "error", "merged", "closed", ""} {
		t.Run(state, func(t *testing.T) {
			d := pageData{attempts: attempts, prChecks: map[string]PRCheck{c.ID: {State: state}}}
			var out bytes.Buffer
			if err := cardNote(d, c).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "Pull request has merge conflicts.") != (state == "conflict") {
				t.Fatalf("state %q: %s", state, out.String())
			}
		})
	}
	if !canAction(Card{ID: c.ID, Status: "Fix"}, []Attempt{{CardID: c.ID, State: "failed", RunState: "failed"}}, "retry") {
		t.Fatal("Fix retry guard")
	}
	if canAction(c, nil, "fix") {
		t.Fatal("legacy Done card can fix")
	}
	if canAction(Card{ID: c.ID, Status: "Fix"}, attempts, "fix") {
		t.Fatal("Fix card can queue another fix")
	}
}

func TestFixNativeAndUnavailable(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "message": {"Fix review feedback"}}
	status(t, request(a, "POST", "/cards/fix", form, session, csrf), http.StatusBadRequest)
	repairDone(t, a.workflow, c)
	status(t, request(a, "POST", "/cards/fix", form, session, csrf), http.StatusSeeOther)
	assertActionLane(t, a.workflow, c.ProjectID, "Fix", c.ID)
	// Only replace the app pointer; preserve the workflow cleanup registered by testApp.
	a.workflow = nil
	status(t, request(a, "POST", "/cards/fix", form, session, csrf), http.StatusBadRequest)
}
