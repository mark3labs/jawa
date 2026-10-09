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
	t.Setenv("NATS_USERNAME", "worker")
	t.Setenv("NATS_PASSWORD", "secret")
	s, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir(), Username: "worker", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}
	t.Cleanup(s.Shutdown)
	cfg := natsConfig()
	cfg.URL, cfg.WorkerID = s.ClientURL(), "worker-1"
	shared, pc, cleanup, err := openNATSPresence(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if shared.Conn == nil || shared.URL != "" || shared.Username != "" || shared.Password != "" || pc.WorkerID != cfg.WorkerID {
		t.Fatal("shared connection configuration incorrect")
	}
	ctx := context.Background()
	rec := presence.Record{Identity: presence.Identity{Worker: pc.WorkerID, Instance: "one"}, State: presence.Ready}
	if err := pc.Registry.Register(ctx, rec); err != nil {
		t.Fatal(err)
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

func TestPresenceDisabledAndHelpDoNotConnect(t *testing.T) {
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
	t.Setenv("JAWA_NATS_PRESENCE", "false")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
	configureServing(cmd, bonnie.New())
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
}

func TestPresenceValidationDoesNotExposeCredentials(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("NATS_TOKEN", "secret-token")
	t.Setenv("NATS_USERNAME", "secret-user")
	t.Setenv("NATS_PASSWORD", "secret-password")
	cfg := natsConfig()
	cfg.WorkerID = "worker-1"
	_, _, _, err := openNATSPresence(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("unsafe or missing validation error: %v", err)
	}
}
