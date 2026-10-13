package orchestrator

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestMutationAdmission(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	a.workflow = nil // Admission must precede workflow availability.
	for path, route := range mutations {
		t.Run(path, func(t *testing.T) {
			if route.run == nil {
				t.Fatal("mutation has no handler")
			}
			w := request(a, http.MethodGet, path, nil, session, csrf)
			status(t, w, http.StatusMethodNotAllowed)
			if w.Header().Get("Allow") != "POST" {
				t.Fatal("missing POST Allow header")
			}
			for _, form := range []url.Values{nil, {"_csrf": {"incorrect"}}} {
				status(t, request(a, http.MethodPost, path, form, session, csrf), http.StatusForbidden)
			}
		})
	}
	// Every workflow route is guarded before entering its handler.
	for path, route := range mutations {
		if !route.needsWorkflow {
			continue
		}
		t.Run("workflow"+path, func(t *testing.T) {
			w := request(a, http.MethodPost, path, url.Values{"_csrf": {csrf.Value}}, session, csrf)
			status(t, w, http.StatusBadRequest)
			if !strings.Contains(w.Body.String(), "workflow") {
				t.Fatalf("missing workflow error: %s", w.Body.String())
			}
		})
	}

	// Before onboarding, CSRF still wins over the NATS gate.
	if _, err := a.s.db.Exec("DELETE FROM settings WHERE key='nats_configured'"); err != nil {
		t.Fatal(err)
	}
	for path, route := range mutations {
		t.Run("onboarding"+path, func(t *testing.T) {
			status(t, request(a, http.MethodPost, path, nil, session, csrf), http.StatusForbidden)
			if !route.allowBeforeNATS {
				status(t, request(a, http.MethodPost, path, url.Values{"_csrf": {csrf.Value}}, session, csrf), http.StatusConflict)
			}
		})
	}
}

func TestMutationPreflightOrder(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	a.workflow = nil
	for _, tc := range []struct {
		path    string
		form    url.Values
		message string
	}{
		{"/cards/move", url.Values{"position": {"-1"}}, "Invalid card position."},
		{"/cards/move", url.Values{"position": {strings.Repeat("9", 100)}}, "Invalid card position."},
		{"/cards/move", nil, "workflow is unavailable"},
		{"/cards/resume", nil, "workflow unavailable"},
		{"/cards/planning/approve", nil, "workflow unavailable"},
		{"/cards/force-stop", nil, "workflow unavailable"},
	} {
		t.Run(tc.path+tc.message, func(t *testing.T) {
			form := tc.form
			if form == nil {
				form = url.Values{}
			}
			form.Set("_csrf", csrf.Value)
			w := request(a, http.MethodPost, tc.path, form, session, csrf)
			status(t, w, http.StatusBadRequest)
			if !strings.Contains(w.Body.String(), tc.message) {
				t.Fatalf("response missing %q: %s", tc.message, w.Body.String())
			}
		})
	}
}

func TestForceReadyOperatorPrecedesWorkflow(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	a.workflow = nil
	// Authentication succeeds using the session, but operator lookup fails.
	if _, err := a.s.db.Exec("DELETE FROM admin"); err != nil {
		t.Fatal(err)
	}
	w := request(a, http.MethodPost, "/cards/force-ready", url.Values{"_csrf": {csrf.Value}}, session, csrf)
	status(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "authenticated operator required") {
		t.Fatalf("operator check was reordered: %s", w.Body.String())
	}
}
