package orchestrator

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDialogErrorDestinations(t *testing.T) {
	a := testApp(t)
	for path, name := range map[string]string{"/projects": "project", "/cards": "card", "/cards/delete": "delete", "/cards/fix": "fix", "/cards/guide": "guide", "/cards/cancel": "cancel", "/cards/force-ready": "ready", "/cards/force-stop": "force", "/cards/reset": "notice"} {
		t.Run(path, func(t *testing.T) {
			for _, operation := range []string{strings.Repeat("a", 32), `evil"]#notice`, ""} {
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"dialog_operation": {operation}}.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				r.Header.Set("Datastar-Request", "true")
				w := httptest.NewRecorder()
				a.responseError(w, r, "<script>\nevent: injected", http.StatusConflict)
				body := assertSSE(t, w)
				target := "notice"
				if name != "notice" {
					target = dialogErrorID(name, operation)
				}
				if !strings.Contains(body, `id="`+target+`"`) || !strings.Contains(body, `role="alert"`) || strings.Contains(body, "datastar-patch-signals") || strings.Contains(body, "<script>") || strings.Contains(body, "\nevent: injected") {
					t.Fatal(body)
				}
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "/cards/delete", nil)
	w := httptest.NewRecorder()
	a.responseError(w, r, "rejected", http.StatusConflict)
	status(t, w, http.StatusConflict)
}

func TestPlanningDeleteDialogError(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	c := workflowCard(t, a.s)
	if err := a.workflow.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	before, err := a.s.Cards(c.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	body := assertSSE(t, sseRequest(a, "POST", "/cards/delete", url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "dialog_operation": {strings.Repeat("b", 32)}}, session, csrf))
	if !strings.Contains(body, `id="delete-error-`+strings.Repeat("b", 32)+`"`) || !strings.Contains(body, "cancel planning before resetting or deleting") || strings.Contains(body, "datastar-patch-signals") {
		t.Fatal(body)
	}
	after, err := a.s.Cards(c.ProjectID)
	if err != nil || len(after) != len(before) || after[0].Status != before[0].Status {
		t.Fatalf("changed card: %v %v", after, err)
	}
	var count int
	if err := a.s.db.QueryRow("SELECT count(*) FROM planning_sessions WHERE card_id=?", c.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost planning session: %d %v", count, err)
	}
}

func TestMutationSuccessRemovesOwnFrameLast(t *testing.T) {
	a := testApp(t)
	for _, path := range []string{"/projects", "/cards", "/cards/delete", "/cards/fix", "/cards/guide", "/cards/cancel", "/cards/force-ready", "/cards/force-stop", "/cards/assign", "/cards/merge"} {
		for _, op := range []string{strings.Repeat("c", 32), "invalid"} {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(url.Values{"dialog_operation": {op}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Datastar-Request", "true")
			w := httptest.NewRecorder()
			a.snapshot(w, r, true, mutationEffects{})
			body := assertSSE(t, w)
			if validOperation(op) && !strings.HasSuffix(body, removeFrame("dialog-"+op)) {
				t.Fatalf("%s: remove must be last: %s", path, body)
			}
			if !validOperation(op) && strings.Contains(body, "data: mode remove") {
				t.Fatal("invalid operation removed a frame")
			}
			if strings.Contains(body, "Open\":false") {
				t.Fatal("global dialog close signal")
			}
		}
	}
}
