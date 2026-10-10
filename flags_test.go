package main

import (
	"testing"

	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/spf13/cobra"
)

func TestServingFlags(t *testing.T) {
	for _, tc := range []struct {
		name, envName, envAgent string
		args                    []string
		wantName, wantAgent     string
	}{
		{name: "defaults", wantName: "jawa"},
		{name: "environment", envName: "custom", envAgent: "agent-env", wantName: "custom", wantAgent: "agent-env"},
		{name: "flags override environment", envName: "custom", envAgent: "agent-env", args: []string{"--name", "flag-agent", "--nats-agent-id", "agent-flag"}, wantName: "flag-agent", wantAgent: "agent-flag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("JAWA_NAME", tc.envName)
			t.Setenv("JAWA_NATS_AGENT_ID", tc.envAgent)
			calls := 0
			root := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { return nil }}
			servingFlags(root, func(name string, cfg natschannel.Config) {
				calls++
				if name != tc.wantName || cfg.AgentID != tc.wantAgent {
					t.Fatalf("got name %q agent %q", name, cfg.AgentID)
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
