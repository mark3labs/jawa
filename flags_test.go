package main

import (
	"testing"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/spf13/cobra"
)

func TestServingFlags(t *testing.T) {
	for _, tc := range []struct {
		name, envName, envWorker string
		args                     []string
		wantName, wantWorker     string
	}{
		{name: "defaults", wantName: "jawa"},
		{name: "environment", envName: "custom", envWorker: "worker-env", wantName: "custom", wantWorker: "worker-env"},
		{name: "flags override environment", envName: "custom", envWorker: "worker-env", args: []string{"--name", "flag-agent", "--nats-worker-id", "worker-flag"}, wantName: "flag-agent", wantWorker: "worker-flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("JAWA_NAME", tc.envName)
			t.Setenv("JAWA_NATS_WORKER_ID", tc.envWorker)
			calls := 0
			root := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
			servingFlags(root, func(name string, cfg natschannel.Config) {
				calls++
				if name != tc.wantName || cfg.WorkerID != tc.wantWorker {
					t.Fatalf("got name %q worker %q", name, cfg.WorkerID)
				}
				if !cfg.TargetedTasks || cfg.RootSubject != "bonnie" {
					t.Fatal("NATS configuration lost")
				}
			})
			if calls != 0 {
				t.Fatal("configuration applied before flag parsing")
			}
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("configuration applied %d times", calls)
			}
		})
	}
}
