package orchestrator

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDashboardOverridesPersistAndReset(t *testing.T) {
	t.Parallel()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := s.SetDashboardOverrides(map[string]string{"listen": "127.0.0.1:9000", "debug": "true"}); err != nil {
		t.Fatal(err)
	}
	values, err := s.DashboardOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if values["listen"] != "127.0.0.1:9000" || values["debug"] != "true" {
		t.Fatalf("unexpected overrides: %#v", values)
	}
	if err := s.ReplaceDashboardOverrides(map[string]string{"forgejo-token": "secret"}); err != nil {
		t.Fatal(err)
	}
	values, err = s.DashboardOverrides()
	if err != nil || len(values) != 1 || values["forgejo-token"] != "secret" {
		t.Fatalf("atomic replacement failed: %#v, %v", values, err)
	}
	if err := s.ClearDashboardOverrides(); err != nil {
		t.Fatal(err)
	}
	values, err = s.DashboardOverrides()
	if err != nil || len(values) != 0 {
		t.Fatalf("expected cleared settings, got %#v, %v", values, err)
	}
}

func TestValidatedListenAddressPort(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"127.0.0.1:0", "[::1]:65535"} {
		if _, _, err := validatedListenAddress(address); err != nil {
			t.Errorf("%q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"127.0.0.1", "127.0.0.1:-1", "127.0.0.1:65536", "127.0.0.1:abc"} {
		if _, _, err := validatedListenAddress(address); err == nil {
			t.Errorf("%q unexpectedly accepted", address)
		}
	}
}

func TestDashboardSettingsAuthCSRFValidationAndSecretDisclosure(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	a.runtimeDefaults = map[string]string{"listen": "127.0.0.1:9001", "nats-listen": "127.0.0.1:4222", "debug": "false", "allow-no-ci": "true"}
	session, _ := setupTest(t, a)
	page := request(a, "GET", "/settings", nil, session)
	status(t, page, 200)
	csrf := cookie(t, page, "csrf")
	if strings.Contains(page.Body.String(), "JAWA_SECRET_SHOULD_NOT_APPEAR") {
		t.Fatal("secret appeared in settings page")
	}
	assertSettingsProviderUX(t, page.Body.String())
	if !strings.Contains(page.Body.String(), `name="auto-resume-failed-runs" value="true" checked>`) {
		t.Fatal("auto-resume not enabled by default")
	}
	invalidAuto := request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "auto-resume-failed-runs": {"yes"}, "debug": {"true"}}, session, csrf)
	status(t, invalidAuto, 400)

	// Reject invalid addresses before any setting is changed.
	bad := request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "listen": {"127.0.0.1:70000"}, "github-token": {"JAWA_SECRET_SHOULD_NOT_APPEAR"}}, session, csrf)
	status(t, bad, 400)
	values, err := a.s.DashboardOverrides()
	if err != nil || len(values) != 0 {
		t.Fatalf("invalid update partially persisted: %#v, %v", values, err)
	}
	// An authenticated request without a valid CSRF token must not save secrets.
	noCSRF := request(a, "POST", "/settings/orchestrator", url.Values{"github-token": {"JAWA_SECRET_SHOULD_NOT_APPEAR"}}, session)
	status(t, noCSRF, 403)
	values, err = a.s.DashboardOverrides()
	if err != nil || len(values) != 0 {
		t.Fatalf("CSRF bypass persisted: %#v, %v", values, err)
	}
	good := request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "listen": {"127.0.0.1:9000"}, "nats-listen": {"127.0.0.1:4222"}, "debug": {"false"}, "allow-no-ci": {"false"}, "auto-resume-failed-runs": {"false"}, "github-token": {"JAWA_SECRET_SHOULD_NOT_APPEAR"}}, session, csrf)
	status(t, good, 303)
	values, err = a.s.DashboardOverrides()
	if err != nil || values["auto-resume-failed-runs"] != "false" || values["debug"] != "false" || values["allow-no-ci"] != "false" || values["github-token"] != "JAWA_SECRET_SHOULD_NOT_APPEAR" {
		t.Fatalf("explicit false or secret override not persisted: %#v, %v", values, err)
	}
	if values["forgejo-token"] != "" {
		t.Fatalf("unexpected Forgejo token override: %#v", values)
	}
	page = request(a, "GET", "/settings", nil, session, csrf)
	status(t, page, 200)
	if strings.Contains(page.Body.String(), "JAWA_SECRET_SHOULD_NOT_APPEAR") {
		t.Fatal("saved secret appeared in settings page")
	}
	if strings.Contains(page.Body.String(), `value="JAWA_SECRET_SHOULD_NOT_APPEAR"`) {
		t.Fatal("saved secret returned in HTML value")
	}
	// A no-op live refresh renders the form too; credentials stay write-only.
	live, err := a.liveSnapshot(httptest.NewRequest("GET", "/events", nil), viewParams{View: viewSettings})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(live, "JAWA_SECRET_SHOULD_NOT_APPEAR") {
		t.Fatal("saved secret appeared in live refresh")
	}
	if strings.Contains(live, `name="auto-resume-failed-runs" value="true" checked>`) {
		t.Fatal("explicit false lost in SSE")
	}
	partial := request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "debug": {"true"}}, session, csrf)
	status(t, partial, 303)
	values, err = a.s.DashboardOverrides()
	if err != nil || values["auto-resume-failed-runs"] != "false" {
		t.Fatal("partial form lost toggle", err)
	}
	reset := request(a, "POST", "/settings/orchestrator/reset", url.Values{"_csrf": {csrf.Value}}, session, csrf)
	status(t, reset, 303)
	live, err = a.liveSnapshot(httptest.NewRequest("GET", "/events", nil), viewParams{View: viewSettings})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(live, `name="auto-resume-failed-runs" value="true" checked>`) || !strings.Contains(live, `name="listen" value="127.0.0.1:9001"`) || !strings.Contains(live, `name="debug" value="true">`) || !strings.Contains(live, `name="allow-no-ci" value="true" checked>`) {
		t.Fatalf("live reset did not restore bootstrap values: %s", live)
	}
	if strings.Contains(live, "JAWA_SECRET_SHOULD_NOT_APPEAR") {
		t.Fatal("cleared secret appeared in live refresh")
	}
}

func assertSettingsProviderUX(t *testing.T, body string) {
	t.Helper()
	for _, want := range []string{`>Save</button>`, `Clear saved GitHub token override`, `Clear saved Forgejo token override`, `github.com`, `codeberg.org`} {
		if !strings.Contains(body, want) {
			t.Errorf("settings page missing expected provider UX %q", want)
		}
	}
	for _, old := range []string{"Clear GitHub token", "Clear Forgejo token", `name="clear-github-token"`, `name="clear-forgejo-token"`} {
		if strings.Contains(body, old) {
			t.Errorf("settings page contains obsolete separate clear control %q", old)
		}
	}
}

func TestDashboardSettingsClearTokenOverridesIndependently(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, _ := setupTest(t, a)
	page := request(a, "GET", "/settings", nil, session)
	csrf := cookie(t, page, "csrf")
	seed := request(a, "POST", "/settings/orchestrator", url.Values{
		"_csrf": {csrf.Value}, "listen": {"127.0.0.1:9100"}, "nats-listen": {"127.0.0.1:4223"},
		"debug": {"false"}, "allow-no-ci": {"true"}, "github-token": {"github-secret"}, "forgejo-token": {"forgejo-secret"},
	}, session, csrf)
	status(t, seed, 303)

	clearGitHub := request(a, "POST", "/settings/orchestrator", url.Values{
		"_csrf": {csrf.Value}, "github-token-clear": {"true"}, "listen": {"127.0.0.1:9200"},
		"nats-listen": {"127.0.0.1:4224"}, "debug": {"true"}, "allow-no-ci": {"false"},
	}, session, csrf)
	status(t, clearGitHub, 303)
	values, err := a.s.DashboardOverrides()
	if err != nil || values["github-token"] != "" || values["forgejo-token"] != "forgejo-secret" {
		t.Fatalf("clearing GitHub changed wrong token: %#v, %v", values, err)
	}
	if values["listen"] != "127.0.0.1:9200" || values["nats-listen"] != "127.0.0.1:4224" || values["debug"] != "true" || values["allow-no-ci"] != "false" {
		t.Fatalf("clearing GitHub did not preserve submitted settings: %#v", values)
	}

	clearForgejo := request(a, "POST", "/settings/orchestrator", url.Values{
		"_csrf": {csrf.Value}, "forgejo-token-clear": {"true"}, "listen": {"127.0.0.1:9300"},
		"nats-listen": {"127.0.0.1:4225"}, "debug": {"false"}, "allow-no-ci": {"true"},
	}, session, csrf)
	status(t, clearForgejo, 303)
	values, err = a.s.DashboardOverrides()
	if err != nil || values["forgejo-token"] != "" || values["github-token"] != "" {
		t.Fatalf("clearing Forgejo changed wrong token: %#v, %v", values, err)
	}
	if values["listen"] != "127.0.0.1:9300" || values["nats-listen"] != "127.0.0.1:4225" || values["debug"] != "false" || values["allow-no-ci"] != "true" {
		t.Fatalf("clearing Forgejo did not preserve submitted settings: %#v", values)
	}
}

func TestDashboardEffectiveValuesHonorRuntimeDefaultsAndReset(t *testing.T) {
	t.Parallel()
	d := pageData{defaults: map[string]string{"listen": "127.0.0.1:9001", "debug": "false", "allow-no-ci": "true"}}
	if got := dashboardValue(d, "listen", "JAWA_ORCHESTRATOR_LISTEN", "127.0.0.1:8080"); got != "127.0.0.1:9001" {
		t.Fatalf("runtime CLI default lost: %q", got)
	}
	if got := dashboardValue(d, "debug", "JAWA_ORCHESTRATOR_DEBUG", "true"); got != "false" {
		t.Fatalf("explicit CLI false lost: %q", got)
	}
	if got := dashboardValue(d, "allow-no-ci", "JAWA_ALLOW_NO_CI", "false"); got != "true" {
		t.Fatalf("no-CI default lost: %q", got)
	}
	d.dashboard = map[string]string{"listen": "127.0.0.1:9999", "debug": "true", "allow-no-ci": "false"}
	if got := dashboardValue(d, "debug", "JAWA_ORCHESTRATOR_DEBUG", "false"); got != "true" {
		t.Fatalf("dashboard override did not win: %q", got)
	}
	d.dashboard = map[string]string{}
	if got := dashboardValue(d, "debug", "JAWA_ORCHESTRATOR_DEBUG", "true"); got != "false" {
		t.Fatalf("reset did not restore runtime default: %q", got)
	}
}
