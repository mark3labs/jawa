// Command serves the agent defined by this tree.
//
// This file is yours; BONNIE never rewrites it. The tree's data lives at its
// default paths — instructions.md is the system prompt, context/ supplies the
// agent's root for files — and the tools under tools/ are wired by codegen
// into bonnie_gen.go. Everything else is an option below.
package main

import (
	"os"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func main() {
	bonnie.New(
		bonnie.WithModel(envOr("JAWA_MODEL", "opencode/glm-5.3-flash")),
		// Default to loopback outside Docker. Container port publishing must
		// restrict access to this unauthenticated API.
		bonnie.WithAddr(envOr("JAWA_HTTP_ADDR", "127.0.0.1:8080")),
		withNATS(natsConfig()),
		bonnie.WithSandboxEnv(codingEnv()),
		bonnie.WithSandboxes(codingSandboxes()...),
		bonnie.WithActivityLogger(bonnie.NewActivityLogger(nil)),
		bonnie.WithRunSandboxCleanup(sandboxCleanupPolicy()),
		bonnie.WithCompletionHook(goCompletionPolicy()),
		bonnie.WithoutHumanInput(),
		bonnie.WithKit(func(o *kit.Options) {
			o.MCPConfig = lightpandaMCPConfig()
		}),

		// Local provides run directories, NOT isolation. Commands inherit the
		// server environment and can access other container files. Only run
		// trusted tasks; use Docker/microVM sandboxes for stronger isolation.
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

func codingSandboxes() []sandbox.Provider {
	return []sandbox.Provider{
		sandbox.Local(sandbox.WithLocalRoot(envOr("JAWA_SANDBOX_ROOT", ".bonnie/workspaces"))),
		sandbox.Microsandbox(
			sandbox.WithMicrosandboxImage(envOr("JAWA_SANDBOX_IMAGE", "ghcr.io/mark3labs/jawa:latest")),
			sandbox.WithMicrosandboxMemory(4096),
			sandbox.WithMicrosandboxCPUs(2),
		),
	}
}

func sandboxCleanupPolicy() bonnie.SandboxCleanupPolicy {
	return bonnie.SandboxCleanupPolicy{
		CompletedAfter: 7 * 24 * time.Hour,
		FailedAfter:    14 * 24 * time.Hour,
		CancelledAfter: 3 * 24 * time.Hour,
		RetiredAfter:   3 * 24 * time.Hour,
	}
}

// Authentication is loaded by WithNATS from NATS_* environment variables,
// not included in model prompts. Local commands inherit these credentials.
func natsConfig() natschannel.Config {
	return natschannel.Config{
		URL:           envOr("NATS_URL", "nats://127.0.0.1:4222"),
		RootSubject:   envOr("JAWA_NATS_ROOT_SUBJECT", "bonnie"),
		WorkerID:      os.Getenv("JAWA_NATS_WORKER_ID"),
		Consumer:      os.Getenv("JAWA_NATS_CONSUMER"),
		CreateStream:  envOr("JAWA_NATS_CREATE_STREAM", "true") == "true",
		TargetedTasks: true,
	}
}

// Work around v0.14.0 WithNATS returning a typed nil on validation failure.
// A genuinely nil interface lets BONNIE report the error instead of panicking
// when it attempts to shut down the channel that was never constructed.
func withNATS(cfg natschannel.Config) bonnie.Option {
	return bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) {
		c := cfg
		if c.Conn == nil {
			for field, key := range map[*string]string{
				&c.URL: "NATS_URL", &c.NKeySeed: "NATS_NKEY_SEED",
				&c.Token: "NATS_TOKEN", &c.Username: "NATS_USERNAME", &c.Password: "NATS_PASSWORD",
			} {
				if *field == "" {
					*field = os.Getenv(key)
				}
			}
		}
		ch, err := natschannel.New(r, c)
		if err != nil {
			return nil, err
		}
		return ch, nil
	})
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

// Explicit coding settings. Local also inherits the server environment,
// including NATS and model-provider credentials; this is not an allowlist.
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
