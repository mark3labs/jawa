package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/presence"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/spf13/cobra"
)

func TestNATSPresenceConnectionAndRegistry(t *testing.T) {
	clearConfigEnv(t)
	for _, key := range []string{"NATS_TOKEN", "NATS_NKEY_SEED", "JAWA_NATS_PRESENCE_BUCKET"} {
		t.Setenv(key, "")
	}
	t.Setenv("NATS_USERNAME", "agent")
	t.Setenv("NATS_PASSWORD", "secret")
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), Username: "agent", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	cfg := natsConfig()
	cfg.URL, cfg.AgentID = s.ClientURL(), "agent-1"
	shared, pc, cleanup, err := openNATSPresence(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if shared.Conn == nil || shared.URL != "" || shared.Username != "" || shared.Password != "" || pc.AgentID != cfg.AgentID {
		t.Fatal("shared connection configuration incorrect")
	}
	ctx := context.Background()
	rec := presence.Record{Identity: presence.Identity{Agent: pc.AgentID, Instance: "one"}, State: presence.Ready}
	if err := pc.Registry.Register(ctx, rec); err != nil {
		t.Fatal(err)
	}
	conflict := rec
	conflict.Identity.Instance = "two"
	if err := pc.Registry.Register(ctx, conflict); err == nil {
		t.Fatal("registration conflict silently accepted")
	}
	rows, err := pc.Registry.(presence.Discoverer).Discover(ctx, presence.Filter{})
	if err != nil || len(rows) != 1 || rows[0].Identity != rec.Identity {
		t.Fatalf("discovery = %v, %v", rows, err)
	}
	if err := pc.Registry.Unregister(ctx, rec.Identity); err != nil {
		t.Fatal(err)
	}
	// Borrowed connections must remain open when the helper's cleanup runs.
	_, _, borrowedCleanup, err := openNATSPresence(ctx, shared)
	if err != nil {
		t.Fatal(err)
	}
	borrowedCleanup()
	if shared.Conn.IsClosed() {
		t.Fatal("closed borrowed connection")
	}
	cleanup()
	if !shared.Conn.IsClosed() {
		t.Fatal("owned connection leaked")
	}
}

func TestServingPresenceMandatory(t *testing.T) {
	for _, enabled := range []string{"", "true", "false"} {
		t.Run("presence-"+enabled, func(t *testing.T) {
			clearConfigEnv(t)
			for _, key := range []string{"NATS_USERNAME", "NATS_PASSWORD", "NATS_TOKEN", "NATS_NKEY_SEED", "JAWA_NATS_PRESENCE_BUCKET"} {
				t.Setenv(key, "")
			}
			t.Setenv("JAWA_NATS_API", "false")
			t.Setenv("JAWA_NATS_PRESENCE", enabled)
			s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			go s.Start()
			if !s.ReadyForConnections(5 * time.Second) {
				t.Fatal("server not ready")
			}
			t.Cleanup(s.Shutdown)
			t.Setenv("NATS_URL", s.ClientURL())
			t.Setenv("JAWA_NATS_AGENT_ID", "agent-1")
			called := false
			cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error {
				called = true
				want := 1
				if got := s.NumClients(); got != want {
					t.Fatalf("presence connections = %d, want %d", got, want)
				}
				return nil
			}}
			configureServing(cmd, bonnie.New())
			cmd.SetArgs(nil)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("serving command did not run")
			}
			deadline := time.Now().Add(5 * time.Second)
			for s.NumClients() != 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if s.NumClients() != 0 {
				t.Fatal("owned connection leaked after serving returned")
			}

		})
	}
}

func TestPresenceHelpDoesNotConnect(t *testing.T) {
	for _, enabled := range []string{"", "true"} {
		t.Run("help-"+enabled, func(t *testing.T) {
			t.Setenv("JAWA_NATS_PRESENCE", enabled)
			t.Setenv("NATS_URL", "nats://127.0.0.1:1")
			cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
			configureServing(cmd, bonnie.New())
			cmd.SetArgs([]string{"--help"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPresenceValidationDoesNotExposeCredentials(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NATS_TOKEN", "secret-token")
	t.Setenv("NATS_USERNAME", "secret-user")
	t.Setenv("NATS_PASSWORD", "secret-password")
	cfg := natsConfig()
	cfg.AgentID = "agent-1"
	_, _, _, err := openNATSPresence(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("unsafe or missing validation error: %v", err)
	}
}

func TestServingModel(t *testing.T) {
	for _, tc := range []struct {
		name, env, flag, provider, model string
		hasFlag                          bool
	}{
		{"default", "", "", "opencode", "glm-5.3-flash", false},
		{"environment", "custom/env", "", "custom", "env", false},
		{"override", "custom/env", "other/flag", "other", "flag", true},
		{"empty-environment", "custom/env", "", "custom", "env", true},
		{"empty-default", "", "", "opencode", "glm-5.3-flash", true},
		{"slash", "custom/org/model", "", "custom", "org/model", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JAWA_MODEL", tc.env)
			cmd := &cobra.Command{}
			if tc.hasFlag {
				cmd.Flags().String("model", tc.flag, "")
			}
			selection, provider, model, err := servingModel(cmd)
			if err != nil || provider != tc.provider || model != tc.model || selection != provider+"/"+model {
				t.Fatalf("model = %q %q %q, %v", selection, provider, model, err)
			}
		})
	}
}

func TestInvalidModelBeforeDial(t *testing.T) {
	for _, selection := range []string{"secret-selection", "secret/", "/secret"} {
		t.Run(selection, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("JAWA_MODEL", selection)
			t.Setenv("NATS_URL", "nats://127.0.0.1:1")
			cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { t.Fatal("ran invalid model"); return nil }}
			configureServing(cmd, bonnie.New())
			cmd.SetArgs(nil)
			err := cmd.Execute()
			if err == nil || err.Error() != "jawa: invalid model selection; expected provider/model" {
				t.Fatalf("unsafe or wrong error: %v", err)
			}
		})
	}
}

func TestPresenceFlagRemoved(t *testing.T) {
	for _, value := range []string{"false", "true"} {
		cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { t.Fatal("ran removed flag"); return nil }}
		configureServing(cmd, bonnie.New())
		cmd.SetArgs([]string{"--nats-presence=" + value})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag: --nats-presence") {
			t.Fatalf("removed flag: %v", err)
		}
	}
}

func TestPresenceStartupFailure(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JAWA_NATS_API", "false")
	t.Setenv("JAWA_NATS_PRESENCE", "false")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	t.Setenv("JAWA_NATS_AGENT_ID", "agent-1")
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { t.Fatal("served without presence"); return nil }}
	configureServing(cmd, bonnie.New())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || err.Error() != "jawa: NATS presence connection failed" {
		t.Fatalf("connection error: %v", err)
	}
}

func TestPresenceBucketFailureClosesOwnedConnection(t *testing.T) {
	clearConfigEnv(t)
	for _, key := range []string{"NATS_USERNAME", "NATS_PASSWORD", "NATS_TOKEN", "NATS_NKEY_SEED"} {
		t.Setenv(key, "")
	}
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	t.Cleanup(s.Shutdown)
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	cfg := natsConfig()
	cfg.URL, cfg.AgentID, cfg.CreateStream = s.ClientURL(), "agent-1", false
	_, _, _, err = openNATSPresenceWithBucket(t.Context(), cfg, "unprovisioned")
	if err == nil || !strings.Contains(err.Error(), "could not open NATS presence bucket") {
		t.Fatalf("bucket error: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.NumClients() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.NumClients() != 0 {
		t.Fatal("owned connection leaked on bucket failure")
	}
}

func TestServingModelAfterPriorHook(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("JAWA_MODEL", "custom/env")
	cmd := &cobra.Command{Use: "jawa", PreRunE: func(cmd *cobra.Command, _ []string) error { return cmd.Flags().Set("model", "secret-invalid") }, RunE: func(*cobra.Command, []string) error { t.Fatal("ran invalid model"); return nil }}
	cmd.Flags().String("model", "custom/initial", "")
	configureServing(cmd, bonnie.New())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || err.Error() != "jawa: invalid model selection; expected provider/model" {
		t.Fatalf("prior hook model not validated: %v", err)
	}
}

func TestNonServingCommandDoesNotDial(t *testing.T) {
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	called := false
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { t.Fatal("served"); return nil }}
	cmd.AddCommand(&cobra.Command{Use: "other", RunE: func(*cobra.Command, []string) error { called = true; return nil }})
	configureServing(cmd, bonnie.New())
	cmd.SetArgs([]string{"other"})
	if err := cmd.Execute(); err != nil || !called {
		t.Fatalf("non-serving command: %v", err)
	}
}
