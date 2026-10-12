package main

import (
	"strings"
	"testing"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/spf13/cobra"
)

func TestConnectionFlagsOverrideEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NATS_PASSWORD", "env-secret")
	t.Setenv("NATS_TOKEN", "env-token")
	t.Setenv("NATS_USERNAME", "env-user")
	t.Setenv("NATS_NKEY_SEED", "env-seed")
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
	var got natschannel.Config
	servingFlags(cmd, func(_ string, cfg natschannel.Config) { got = cfg })
	cmd.SetArgs([]string{"--nats-url=nats://example:4223", "--nats-root-subject=custom", "--nats-consumer=workers", "--nats-create-stream=false", "--nats-password=flag-secret", "--nats-token=", "--nats-username=flag-user", "--nats-nkey-seed="})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.URL != "nats://example:4223" || got.RootSubject != "custom" || got.Consumer != "workers" || got.CreateStream || got.Password != "flag-secret" || got.Username != "flag-user" || got.Token != "" || got.NKeySeed != "" {
		t.Fatalf("flag overrides not applied")
	}
	if !got.TargetedTasks {
		t.Fatal("targeted tasks lost")
	}
}

func TestServingConfigurationFlags(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("GITHUB_TOKEN", "secret-github-default")
	t.Setenv("NATS_PASSWORD", "secret-nats-default")
	t.Setenv("JAWA_NATS_PRESENCE", "true")
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
	configureServing(cmd, bonnie.New())
	cmd.SetArgs([]string{"--nats-presence=false", "--nats-agent-id=test-agent", "--sandbox-root=/tmp/custom", "--sandbox-image=custom:image", "--github-token=", "--provider-api-key=flag-key", "--provider-url=http://localhost:8000"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"nats-presence-bucket", "sandbox-root", "sandbox-image", "github-token", "provider-api-key", "provider-url"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("missing flag %s", flag)
		}
	}
	help := cmd.UsageString()
	if strings.Contains(help, "secret-github-default") || strings.Contains(help, "secret-nats-default") {
		t.Fatal("help leaked credentials")
	}
}
