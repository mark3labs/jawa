package orchestrator

import (
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jawa/internal/agentconcurrency"

	"github.com/nats-io/nats.go"
)

func concurrencyKV(t *testing.T, nc *nats.Conn) nats.KeyValue {
	t.Helper()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(agentconcurrency.Bucket)
	if err != nil {
		t.Fatal(err)
	}
	return kv
}

func assertConcurrencyValue(t *testing.T, kv nats.KeyValue, want string) {
	t.Helper()
	entry, err := kv.Get(agentconcurrency.Key)
	if want == "" {
		if !errors.Is(err, nats.ErrKeyNotFound) && !errors.Is(err, nats.ErrKeyDeleted) {
			t.Fatalf("expected no override, got %v, %v", entry, err)
		}
	} else if err != nil || string(entry.Value()) != want {
		t.Fatalf("wanted override %s, got %v, %v", want, entry, err)
	}
}

func TestAgentConcurrencyHTTP(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, _ := setupTest(t, a)
	page := request(a, "GET", "/settings", nil, session)
	csrf := cookie(t, page, "csrf")
	kv := concurrencyKV(t, a.workflow.resumeConn)
	assertConcurrencyValue(t, kv, "")
	for _, fragment := range []string{`for="runtime-agent-concurrency"`, `name="agent-concurrency"`, `type="number"`, `min="1"`, `max="1024"`, `agent-concurrency-help`, `each agent uses --concurrency`} {
		if !strings.Contains(page.Body.String(), fragment) {
			t.Errorf("UI missing %q", fragment)
		}
	}
	post := func(value string) {
		t.Helper()
		status(t, request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "agent-concurrency": {value}}, session, csrf), 303)
	}
	for _, invalid := range []string{"0", "-1", "1025", "1.5", "abc", "1e2"} {
		status(t, request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "agent-concurrency": {invalid}, "debug": {"true"}}, session, csrf), 400)
		values, err := a.s.DashboardOverrides()
		if err != nil || len(values) != 0 {
			t.Fatalf("invalid request persisted: %v, %v", values, err)
		}
		assertConcurrencyValue(t, kv, "")
	}
	status(t, request(a, "POST", "/settings/orchestrator", url.Values{"agent-concurrency": {"2"}}, session), 403)
	for _, valid := range []string{"1", strconv.Itoa(agentconcurrency.Max), "7"} {
		post(valid)
		values, err := a.s.DashboardOverrides()
		if err != nil || values["agent-concurrency"] != valid {
			t.Fatalf("save: %v, %v", values, err)
		}
		assertConcurrencyValue(t, kv, valid)
	}
	status(t, request(a, "POST", "/settings/orchestrator", url.Values{"_csrf": {csrf.Value}, "debug": {"true"}}, session, csrf), 303)
	assertConcurrencyValue(t, kv, "7")
	page = request(a, "GET", "/settings", nil, session, csrf)
	if !strings.Contains(page.Body.String(), `name="agent-concurrency"`) || !strings.Contains(page.Body.String(), `value="7"`) {
		t.Fatal("UI lost saved override")
	}
	post("")
	values, err := a.s.DashboardOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := values["agent-concurrency"]; ok {
		t.Fatal("blank did not remove persisted override")
	}
	assertConcurrencyValue(t, kv, "")
	post("3")
	status(t, request(a, "POST", "/settings/orchestrator/reset", url.Values{"_csrf": {csrf.Value}}, session, csrf), 303)
	assertConcurrencyValue(t, kv, "")
}

func TestAgentConcurrencyStartupAndSync(t *testing.T) {
	t.Parallel()
	s, nc := workflowFixture(t)
	if err := s.SetDashboardOverrides(map[string]string{"agent-concurrency": "6"}); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	kv := concurrencyKV(t, nc)
	assertConcurrencyValue(t, kv, "6")
	before, err := kv.Get(agentconcurrency.Key)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.syncAgentConcurrency(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := kv.Get(agentconcurrency.Key)
	if err != nil || before.Revision() != after.Revision() {
		t.Fatal("unchanged sync wrote a new revision")
	}
	// The periodic reconciler repairs missed publications from persisted state.
	if _, err := kv.Put(agentconcurrency.Key, []byte("9")); err != nil {
		t.Fatal(err)
	}
	if err := w.syncAgentConcurrency(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertConcurrencyValue(t, kv, "6")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearDashboardOverrides(); err != nil {
		t.Fatal(err)
	}
	w, err = NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	assertConcurrencyValue(t, kv, "")
}

func TestAgentConcurrencyPersistence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDashboardOverrides(map[string]string{"agent-concurrency": "8"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
	values, err := s.DashboardOverrides()
	if err != nil || values["agent-concurrency"] != "8" {
		t.Fatalf("reopened override: %v, %v", values, err)
	}
}
