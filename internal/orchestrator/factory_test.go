package orchestrator

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestWorkflowProviderVerifierToDone(t *testing.T) {
	t.Setenv("JAWA_GITHUB_TOKEN", "test-secret")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("JAWA_ALLOW_NO_CI", "")
	s, nc := workflowFixture(t)
	// testProject creates an unconfigured project; workflowCard supplies the
	// real GitHub repository/base branch required by the production verifier.
	card := workflowCard(t, s)
	branch := fmt.Sprintf("jawa/card/%s/attempt/1", card.ID)
	sha := strings.Repeat("a", 40)
	root := "/repos/example/repo"
	prPath := root + "/pulls/7"
	prURL := "https://github.com/example/repo/pull/7"

	// These are the successful provider_test fixtures, with the repository and
	// deterministic workflow branch substituted. Only CI starts non-green.
	var mu sync.Mutex
	green := false
	calls := make(map[string]int)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.Path]++
		ciGreen := green
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("incorrect provider authorization")
		}
		w.Header().Set("Content-Type", "application/json")
		var body string
		switch r.URL.Path {
		case prPath:
			body = fmt.Sprintf(`{"number":7,"state":"open","mergeable":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"example/repo"}},"base":{"ref":"main","repo":{"full_name":"example/repo"}}}`, branch, sha)
		case root + "/branches/main":
			body = `{"protected":true}`
		case root + "/branches/main/protection/required_status_checks":
			body = `{"contexts":["build"],"checks":[{"context":"build","app_id":1}]}`
		case root + "/rules/branches/main":
			body = `[]`
		case root + "/commits/" + sha + "/check-runs":
			state := "in_progress"
			if ciGreen {
				state = "completed"
			}
			body = fmt.Sprintf(`{"total_count":1,"check_runs":[{"name":"build","head_sha":%q,"status":%q,"conclusion":"success","app":{"id":1}}]}`, sha, state)
		case root + "/commits/" + sha + "/status":
			body = fmt.Sprintf(`{"sha":%q,"state":"pending","total_count":0,"statuses":[]}`, sha)
		case prPath + "/reviews":
			body = `[]`
		case "/graphql":
			if r.Method != http.MethodPost {
				t.Error("review threads must be queried via POST")
			}
			var query struct {
				Variables struct {
					Owner  string `json:"owner"`
					Name   string `json:"name"`
					Number int    `json:"number"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
				t.Errorf("decode GraphQL request: %v", err)
			}
			if query.Variables.Owner != "example" || query.Variables.Name != "repo" || query.Variables.Number != 7 {
				t.Errorf("wrong review thread target: %+v", query.Variables)
			}
			body = fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"headRefOid":%q,"reviewThreads":{"nodes":[{"isResolved":true}],"pageInfo":{"hasNextPage":false}}}}}}`, sha)
		default:
			t.Errorf("unexpected provider request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("write provider fixture: %v", err)
		}
	}))
	defer provider.Close()
	transport := provider.Client().Transport
	httpClient := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
			return nil, fmt.Errorf("unexpected credential destination: %s", r.URL)
		}
		copied := r.Clone(r.Context())
		u := *r.URL
		copied.URL = &u
		u.Host = strings.TrimPrefix(provider.URL, "https://")
		return transport.RoundTrip(copied)
	})}

	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	w.SetVerifier(ProviderVerifier(s, httpClient))
	if err := w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}

	// Act as a worker on the actual task subject, not a constructed TaskJSON.
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	sub, err := js.PullSubscribe("bonnie.tasks", "factory-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := sub.Unsubscribe(); err != nil {
			t.Error(err)
		}
	}()
	msgs, err := sub.Fetch(1, nats.MaxWait(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var task client.Task
	if err := json.Unmarshal(msgs[0].Data, &task); err != nil {
		t.Fatal(err)
	}
	if err := msgs[0].AckSync(); err != nil {
		t.Fatal(err)
	}
	attempt, err := w.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.TaskID != attempt.TaskID || task.Version != 1 || attempt.Number != 1 || attempt.TaskJSON != string(msgs[0].Data) {
		t.Fatalf("published task differs from immutable attempt: task=%+v attempt=%+v", task, attempt)
	}
	if strings.Count(task.Text, "\nBranch: ") != 1 || !strings.HasSuffix(task.Text, "\nBranch: "+branch) {
		t.Fatalf("task lacks matching deterministic branch: %q", task.Text)
	}
	target := client.Target{TaskID: task.TaskID, WorkerID: "worker", RunID: "run", AttemptID: "remote"}
	event := client.StatusEvent{Version: 1, Target: target, EventID: "accepted", Type: "task_accepted", Seq: 0, State: runtime.RunRunning}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.Publish("bonnie.events", data); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		a, err := w.CardResult(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a.State == "running" && a.WorkerID == target.WorkerID && a.RunID == target.RunID && a.RemoteAttemptID == target.AttemptID
	})
	publishOutcome(t, nc, client.Outcome{Version: 1, TaskID: task.TaskID, WorkerID: target.WorkerID, RunID: target.RunID, AttemptID: target.AttemptID, State: runtime.RunCompleted, Response: fmt.Sprintf(`{"pr_url":%q}`, prURL)})
	waitWorkflow(t, func() bool {
		a, err := w.CardResult(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		return a.PRURL == prURL && a.RunState == "completed" && a.OutcomeJSON != ""
	})

	assertState := func(ready bool, state, lane string) Attempt {
		t.Helper()
		a, err := w.CardResult(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		gotReady, err := w.Ready(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		cards, err := s.Cards(card.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		if a.Ready != ready || gotReady != ready || a.State != state || len(cards) != 1 || cards[0].ID != card.ID || cards[0].Status != lane {
			t.Fatalf("want ready=%v state=%s lane=%s; attempt=%+v ready=%v cards=%+v", ready, state, lane, a, gotReady, cards)
		}
		return a
	}
	assertState(false, "blocked", "Building")
	if err := w.MoveCard(card.ID, "Done", 0); err == nil {
		t.Fatal("unverified worker result allowed manual Done")
	}

	// Drive retries directly after result consumption, without the 10s ticker.
	w.verifyPending()
	pending := assertState(false, "blocked", "Building")
	if !strings.Contains(pending.Error, "required CI check missing or unsuccessful") {
		t.Fatalf("pending CI was not the verification blocker: %q", pending.Error)
	}
	mu.Lock()
	if calls[root+"/commits/"+sha+"/check-runs"] == 0 {
		t.Error("pending verification did not read current-head CI")
	}
	green = true
	// Count the green pass separately to prove it re-reads all evidence.
	calls = make(map[string]int)
	mu.Unlock()
	w.verifyPending()
	ready := assertState(true, "ready", "Done")
	if ready.Error != "" || ready.TaskJSON != attempt.TaskJSON || ready.OutcomeJSON != pending.OutcomeJSON || ready.PRURL != prURL {
		t.Fatalf("verification lost immutable task/result or left an error: %+v", ready)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{prPath, root + "/branches/main", root + "/branches/main/protection/required_status_checks", root + "/rules/branches/main", root + "/commits/" + sha + "/check-runs", root + "/commits/" + sha + "/status", prPath + "/reviews", "/graphql"} {
		if calls[path] == 0 {
			t.Errorf("green verification omitted %s", path)
		}
	}
	if calls[prPath] < 2 {
		t.Error("green verification did not recheck PR head after CI and reviews")
	}
}
