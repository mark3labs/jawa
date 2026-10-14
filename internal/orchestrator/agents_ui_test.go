package orchestrator

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

func TestAgentModelSummary(t *testing.T) {
	for _, tc := range []struct {
		name            string
		labels          map[string]string
		provider, model string
	}{
		{"populated", map[string]string{"jawa.provider": "custom", "jawa.model": "org/model"}, "custom", "org/model"},
		{"legacy", nil, "Unknown", "Unknown"},
		{"partial", map[string]string{"jawa.provider": "custom", "jawa.model": ""}, "custom", "Unknown"},
		{"model-only", map[string]string{"jawa.model": "model"}, "Unknown", "model"},
		{"untrusted", map[string]string{"jawa.provider": "<script>&", "jawa.model": "<img>&"}, "&lt;script&gt;&amp;", "&lt;img&gt;&amp;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := presence.Record{Identity: presence.Identity{Agent: "fixture", Instance: "one"}, State: presence.Ready, Labels: tc.labels, Endpoints: []presence.Endpoint{{Channel: "nats", Address: "bonnie.tasks", Input: true, Ready: true}}}
			var out bytes.Buffer
			if err := agentCard(pageData{}, rec).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			html := out.String()
			for _, want := range []string{"Provider: <span>" + tc.provider, "Model: <span>" + tc.model, "Available", "bonnie.tasks", "Inspect record"} {
				if !strings.Contains(html, want) {
					t.Errorf("missing %q", want)
				}
			}
			if strings.Contains(html, "<script>") || strings.Contains(html, "<img>") {
				t.Fatal("unescaped model metadata")
			}
		})
	}
	if strings.Contains(connectSnippet("", ""), "JAWA_NATS_PRESENCE=") {
		t.Fatal("obsolete opt-out in snippet")
	}
}

func TestActivityPresenceLabels(t *testing.T) {
	a := testApp(t)
	session, _ := setupTest(t, a)
	labels := map[string]string{"jawa.provider": "custom", "jawa.model": "org/model", "jawa.api_subject": "bonnie.api.agent.fixture"}
	rec := presence.Record{Identity: presence.Identity{Agent: "fixture", Instance: "one"}, State: presence.Ready, Labels: labels}
	if err := a.workflow.presence.Register(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	response := request(a, "GET", "/activity", nil, session)
	status(t, response, 200)
	var snapshot struct{ Agents []presence.Record }
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Agents) != 1 {
		t.Fatalf("agents = %v", snapshot.Agents)
	}
	for key, value := range labels {
		if snapshot.Agents[0].Labels[key] != value {
			t.Errorf("lost label %s", key)
		}
	}
	status(t, request(a, "GET", "/activity", nil), 303)
}

// Shared component fixture exports a real Agents page and a keyed replacement.
func TestAgentsComponentFixture(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rec := presence.Record{Identity: presence.Identity{Agent: "fixture", Instance: "one"}, State: presence.Ready, UpdatedAt: now, Labels: map[string]string{"jawa.provider": "custom", "jawa.model": "org/model"}, Endpoints: []presence.Endpoint{{Channel: "nats", Address: "bonnie.tasks", Input: true, Ready: true}}}
	d := pageData{v: viewParams{View: viewAgents}, now: now, presence: true, agents: []presence.Record{rec}, projects: []Project{{ID: "p", Name: "Jawa"}}, cards: []Card{{ID: "long", ProjectID: "p", Title: strings.Repeat("Long run title ", 30), Status: "Building"}}, attempts: []Attempt{{ID: "run", CardID: "long", AgentID: "fixture", State: "running", RunState: "running"}}}
	var out bytes.Buffer
	if err := appPage(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "org/model") || !strings.Contains(out.String(), "Long run title") {
		t.Fatal("missing agent fixture fields")
	}
	path := os.Getenv("JAWA_COMPONENT_FIXTURE")
	if path != "" {
		if err := os.WriteFile(path+".agents", out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d.agents[0].Labels = map[string]string{"jawa.provider": "replacement", "jawa.model": "new/slashed/model"}
	out.Reset()
	if err := agentsContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new/slashed/model") || strings.Contains(out.String(), "org/model") {
		t.Fatal("presence labels did not replace")
	}
	if path != "" {
		if err := os.WriteFile(path+".agents-patch", out.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
