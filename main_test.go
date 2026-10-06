package main

import "testing"

func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"NATS_URL", "JAWA_NATS_TASK_SUBJECT", "JAWA_NATS_ANSWER_SUBJECT", "JAWA_NATS_RESULT_SUBJECT", "JAWA_NATS_STREAM", "JAWA_NATS_WORKER_ID", "JAWA_NATS_CONSUMER"} {
		t.Setenv(key, "")
	}
}

func TestNATSDefaults(t *testing.T) {
	clearConfigEnv(t)
	cfg := natsConfig()
	if cfg.URL != "nats://127.0.0.1:4222" || cfg.Subject != "bonnie.tasks.requests" || cfg.AnswerSubject != "bonnie.tasks.answers" || cfg.ResultSubject != "bonnie.tasks.results" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Stream != "" || cfg.WorkerID != "" || cfg.Consumer != "" || cfg.CreateStream {
		t.Fatal("default must use Core NATS without creating streams")
	}
}

func TestNATSOverrides(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NATS_URL", "nats://localhost:4223")
	t.Setenv("JAWA_NATS_TASK_SUBJECT", "test.tasks")
	t.Setenv("JAWA_NATS_ANSWER_SUBJECT", "test.answers")
	t.Setenv("JAWA_NATS_RESULT_SUBJECT", "test.results")
	t.Setenv("JAWA_NATS_STREAM", "JAWA_INPUTS")
	t.Setenv("JAWA_NATS_WORKER_ID", "jawa-1")
	t.Setenv("JAWA_NATS_CONSUMER", "jawa-workers")
	cfg := natsConfig()
	if cfg.URL != "nats://localhost:4223" || cfg.Subject != "test.tasks" || cfg.AnswerSubject != "test.answers" || cfg.ResultSubject != "test.results" || cfg.Stream != "JAWA_INPUTS" || cfg.WorkerID != "jawa-1" || cfg.Consumer != "jawa-workers" {
		t.Fatalf("environment overrides not applied: %+v", cfg)
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
