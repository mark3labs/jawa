package main

import (
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"testing"
)

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"NATS_URL", "JAWA_NATS_ROOT_SUBJECT", "JAWA_NATS_WORKER_ID", "JAWA_NATS_CONSUMER", "JAWA_NATS_CREATE_STREAM"} {
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
	t.Setenv("JAWA_NATS_WORKER_ID", "jawa-1")
	t.Setenv("JAWA_NATS_CONSUMER", "jawa-workers")
	t.Setenv("JAWA_NATS_CREATE_STREAM", "true")
	cfg := natsConfig()
	if cfg.URL != "nats://localhost:4223" || cfg.RootSubject != "test.agent" || cfg.WorkerID != "jawa-1" || cfg.Consumer != "jawa-workers" || !cfg.CreateStream {
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

func TestLightpandaMCPConfig(t *testing.T) {
	cfg := lightpandaMCPConfig()
	server, ok := cfg.MCPServers["lightpanda"]
	if !ok || len(cfg.MCPServers) != 1 {
		t.Fatal("expected a single Lightpanda MCP server")
	}
	if server.Type != "local" || len(server.Command) != 2 || server.Command[0] != "lightpanda" || server.Command[1] != "mcp" {
		t.Fatalf("unexpected MCP command: %+v", server)
	}
}

func TestCodingEnv(t *testing.T) {
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

func TestModelOverride(t *testing.T) {
	t.Setenv("JAWA_MODEL", "")
	if got := envOr("JAWA_MODEL", "default-model"); got != "default-model" {
		t.Fatalf("empty setting: got %q", got)
	}
	t.Setenv("JAWA_MODEL", "provider/model")
	if got := envOr("JAWA_MODEL", "default-model"); got != "provider/model" {
		t.Fatalf("override: got %q", got)
	}
}
