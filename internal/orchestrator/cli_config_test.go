package orchestrator

import (
	"bytes"
	"github.com/spf13/cobra"
	"os"
	"strings"
	"testing"
)

func TestProviderFlagsDefaultsPrecedenceAndRedaction(t *testing.T) {
	t.Setenv("JAWA_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "env-secret")
	t.Setenv("JAWA_FORGEJO_TOKEN", "forgejo-secret")
	t.Setenv("JAWA_ALLOW_NO_CI", "true")
	for _, cmd := range []*cobra.Command{Command(), ReconcileCommand()} {
		if got, _ := cmd.Flags().GetString("github-token"); got != "env-secret" {
			t.Fatalf("default = %q", got)
		}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Help(); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "env-secret") || strings.Contains(out.String(), "forgejo-secret") {
			t.Fatal("help leaked credentials")
		}
		if err := cmd.ParseFlags([]string{"--github-token=cli-secret", "--forgejo-token=", "--allow-no-ci=false"}); err != nil {
			t.Fatal(err)
		}
		defaults := map[string]string{}
		readProviderFlags(cmd, defaults)
		s, err := OpenStore(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		s.runtimeDefaults = defaults
		settings, err := s.providerSettings()
		if err != nil {
			t.Fatal(err)
		}
		if effectiveProviderSetting(settings, "github-token", "JAWA_GITHUB_TOKEN") != "cli-secret" || settings["allow-no-ci"] != "false" {
			t.Fatal("CLI defaults ignored")
		}
		if effectiveProviderSetting(settings, "forgejo-token", "JAWA_FORGEJO_TOKEN") != "" {
			t.Fatal("explicit empty credential ignored")
		}
		if err := s.SetDashboardOverrides(map[string]string{"github-token": "dashboard-secret", "allow-no-ci": "true"}); err != nil {
			t.Fatal(err)
		}
		settings, err = s.providerSettings()
		if err != nil || settings["github-token"] != "dashboard-secret" || settings["allow-no-ci"] != "true" {
			t.Fatal("dashboard precedence lost")
		}
		if err := s.ClearDashboardOverrides(); err != nil {
			t.Fatal(err)
		}
		settings, _ = s.providerSettings()
		if settings["github-token"] != "cli-secret" {
			t.Fatal("reset lost CLI default")
		}
		_ = s.Close()
	}
	if os.Getenv("GITHUB_TOKEN") != "env-secret" || os.Getenv("JAWA_ALLOW_NO_CI") != "true" {
		t.Fatal("flags mutated environment")
	}
}

func TestReconcileConnectionFlags(t *testing.T) {
	t.Setenv("JAWA_ORCHESTRATOR_DATA_DIR", "/tmp/env-dir")
	t.Setenv("NATS_URL", "nats://user:url-secret@127.0.0.1:1")
	t.Setenv("NATS_USERNAME", "env-user")
	t.Setenv("NATS_PASSWORD", "password-secret")
	cmd := ReconcileCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	_ = cmd.Help()
	if strings.Contains(out.String(), "url-secret") || strings.Contains(out.String(), "password-secret") {
		t.Fatal("help leaked connection credentials")
	}
	if got, _ := cmd.Flags().GetString("data-dir"); got != "/tmp/env-dir" {
		t.Fatal("data-dir env ignored")
	}
	cmd.SetArgs([]string{"--task=t", "--run=r", "--attempt=a", "--nats-password=cli-secret"})
	err := cmd.Execute()
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
	if os.Getenv("NATS_PASSWORD") != "password-secret" {
		t.Fatal("environment mutated")
	}
}
