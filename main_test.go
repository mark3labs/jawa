package main

import (
	"testing"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/sandbox"
)

func TestCodingSandboxes(t *testing.T) {
	t.Setenv("JAWA_SANDBOX_ROOT", "")
	t.Setenv("JAWA_SANDBOX_IMAGE", "")
	providers := codingSandboxes()
	if len(providers) != 2 || providers[0].Name() != "local" || providers[1].Name() != "microsandbox" {
		t.Fatal("expected local default and microsandbox option")
	}
	image := providers[1].(sandbox.Imaged).Image()
	if image != "ghcr.io/mark3labs/jawa:latest" {
		t.Fatalf("unexpected guest image %q", image)
	}
}

func TestSandboxImageOverride(t *testing.T) {
	t.Setenv("JAWA_SANDBOX_IMAGE", "jawa:local")
	if codingSandboxes()[1].(sandbox.Imaged).Image() != "jawa:local" {
		t.Fatal("guest image override ignored")
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"NATS_URL", "JAWA_NATS_ROOT_SUBJECT", "JAWA_NATS_AGENT_ID", "JAWA_NATS_CONSUMER", "JAWA_NATS_CREATE_STREAM", "JAWA_DECISION_MODEL"} {
		t.Setenv(key, "")
	}
}

func TestNATSDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := natsConfig()
	if cfg.URL != "nats://127.0.0.1:4222" || cfg.RootSubject != "bonnie" || !cfg.CreateStream || !cfg.TargetedTasks {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	subjects, err := natschannel.ResolveSubjects(cfg.RootSubject, natschannel.Subjects{})
	if err != nil {
		t.Fatal(err)
	}
	if subjects.Tasks != "bonnie.tasks" || subjects.Results != "bonnie.results" || subjects.Events != "bonnie.events" || subjects.Answers != "bonnie.answers" || subjects.Commands != "bonnie.commands" || subjects.Queries != "bonnie.queries" {
		t.Fatalf("unexpected subjects: %+v", subjects)
	}
}

func TestNATSOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NATS_URL", "nats://localhost:4223")
	t.Setenv("JAWA_NATS_ROOT_SUBJECT", "test.agent")
	t.Setenv("JAWA_NATS_AGENT_ID", "jawa-1")
	t.Setenv("JAWA_NATS_CONSUMER", "jawa-agents")
	t.Setenv("JAWA_NATS_CREATE_STREAM", "true")
	cfg := natsConfig()
	if cfg.URL != "nats://localhost:4223" || cfg.RootSubject != "test.agent" || cfg.AgentID != "jawa-1" || cfg.Consumer != "jawa-agents" || !cfg.CreateStream {
		t.Fatalf("environment overrides not applied: %+v", cfg)
	}
}

func TestNATSStreamCreationDisabled(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JAWA_NATS_CREATE_STREAM", "false")
	if natsConfig().CreateStream {
		t.Fatal("explicit false must disable stream creation")
	}
}

// TestCodingEnv verifies tool settings and omission of empty credentials.
func TestCodingEnv(t *testing.T) {
	t.Setenv("JAWA_GIT_USER_NAME", "")
	t.Setenv("JAWA_GIT_USER_EMAIL", "")
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("NATS_PASSWORD", "broker-secret")
	env := codingEnv()
	if env["GITHUB_TOKEN"] != "test-token" || env["GH_PROMPT_DISABLED"] != "1" {
		t.Fatal("GitHub CLI environment not configured")
	}
	if _, ok := env["NATS_PASSWORD"]; ok {
		t.Fatal("broker credentials must not enter sandbox")
	}
	t.Setenv("GITHUB_TOKEN", "")
	if _, ok := codingEnv()["GITHUB_TOKEN"]; ok {
		t.Fatal("empty token should not be injected")
	}
}

// TestCodingEnvForgejoToken verifies the Forgejo credential precedence and name.
func TestCodingEnvForgejoToken(t *testing.T) {
	t.Setenv("JAWA_FORGEJO_TOKEN", "")
	t.Setenv("FORGEJO_TOKEN", "")
	if _, ok := codingEnv()["FORGEJO_TOKEN"]; ok {
		t.Fatal("empty token should not be injected")
	}
	t.Setenv("FORGEJO_TOKEN", "fallback-token")
	if got := codingEnv()["FORGEJO_TOKEN"]; got != "fallback-token" {
		t.Fatalf("fallback = %q", got)
	}
	t.Setenv("JAWA_FORGEJO_TOKEN", "scoped-token")
	env := codingEnv()
	if env["FORGEJO_TOKEN"] != "scoped-token" {
		t.Fatalf("precedence = %q", env["FORGEJO_TOKEN"])
	}
	if _, ok := env["JAWA_FORGEJO_TOKEN"]; ok {
		t.Fatal("sandbox should receive only FORGEJO_TOKEN")
	}
}
