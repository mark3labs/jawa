// Command serves the agent defined by this tree.
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, workspace/ is the
// agent's root for files — and the tools under tools/ are wired by codegen
// into bonnie_gen.go. Everything else is an option below.
package main

import (
	"os"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	bonnie.New(
		bonnie.WithModel(envOr("JAWA_MODEL", "opencode/glm-5.3-flash")),
		// Default to loopback outside Docker. Container port publishing must
		// restrict access to this unauthenticated API.
		bonnie.WithAddr(envOr("JAWA_HTTP_ADDR", "127.0.0.1:8080")),
		bonnie.WithNATS(natsConfig()),
		bonnie.WithSandboxEnv(codingEnv()),
		bonnie.WithActivityLogger(bonnie.NewActivityLogger(nil)),
		bonnie.WithRunWorkspaceCleanup(workspaceCleanupPolicy()),
		bonnie.WithCompletionHook(goCompletionPolicy()),
		bonnie.WithoutHumanInput(),
		bonnie.WithKit(func(o *kit.Options) {
			o.MCPConfig = lightpandaMCPConfig()
		}),

		// Every tool call runs in a sandbox. The default is landlock, which
		// confines tool calls to the run's own workspace and needs nothing
		// installed — it confines the filesystem and the environment, not the
		// network. Uncomment for stronger isolation, or to cut egress.
		//
		//	bonnie.WithSandbox(sandbox.Docker()),
		//	bonnie.WithNetwork(sandbox.NetworkPolicy{Mode: sandbox.NetworkDenyAll}),

		// Chat channels mount beside the HTTP channel. Their credentials
		// come from the environment, never from this file.
		//
		//	bonnie.WithSlack(slack.Config{}),
		//	bonnie.WithDiscord(discord.Config{}),
		//	bonnie.WithTelegram(telegram.Config{Username: "mybot"}),
		//	bonnie.WithGitHub(github.Config{BotName: "mybot"}),
	).Serve()
}

func workspaceCleanupPolicy() bonnie.WorkspaceCleanupPolicy {
	return bonnie.WorkspaceCleanupPolicy{
		CompletedAfter: 7 * 24 * time.Hour,
		FailedAfter:    14 * 24 * time.Hour,
		CancelledAfter: 3 * 24 * time.Hour,
		RetiredAfter:   3 * 24 * time.Hour,
	}
}

// Authentication is loaded by WithNATS from NATS_* environment variables,
// never passed to the model or injected into its sandbox.
func natsConfig() natschannel.Config {
	return natschannel.Config{
		URL:          envOr("NATS_URL", "nats://127.0.0.1:4222"),
		RootSubject:  envOr("JAWA_NATS_ROOT_SUBJECT", "bonnie.tasks"),
		WorkerID:     os.Getenv("JAWA_NATS_WORKER_ID"),
		Consumer:     os.Getenv("JAWA_NATS_CONSUMER"),
		CreateStream: os.Getenv("JAWA_NATS_CREATE_STREAM") == "true",
	}
}

// Lightpanda uses local stdio transport; KIT manages the MCP subprocess.
func lightpandaMCPConfig() *kit.Config {
	return &kit.Config{
		MCPServers: map[string]kit.MCPServerConfig{
			"lightpanda": {
				Type:        "local",
				Command:     []string{"lightpanda", "mcp"},
				Environment: map[string]string{"LIGHTPANDA_DISABLE_TELEMETRY": "true"},
			},
		},
	}
}

// Explicitly allow only the GitHub credential into coding commands. NATS and
// model-provider credentials must stay in the server process.
func codingEnv() map[string]string {
	env := map[string]string{"GH_PROMPT_DISABLED": "1", "LIGHTPANDA_DISABLE_TELEMETRY": "true"}
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		env["GITHUB_TOKEN"] = token
	}
	return env
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
