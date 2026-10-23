package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/spf13/cobra"
)

const liveDecisionModel = "openrouter/typesafe/jev-1.14"

// configuredAgent applies serving configuration. The unreachable NATS URL stops
// the command after the decision model is configured and before it serves.
func configuredAgent(t *testing.T, decisionEnv string, args ...string) (*bonnie.Agent, error) {
	t.Helper()
	clearConfigEnv(t)
	t.Setenv("JAWA_DECISION_MODEL", decisionEnv)
	t.Setenv("JAWA_NATS_API", "false")
	t.Setenv("NATS_URL", "nats://127.0.0.1:1")
	t.Setenv("JAWA_NATS_AGENT_ID", "agent-1")
	agent := bonnie.New()
	cmd := &cobra.Command{Use: "jawa", RunE: func(*cobra.Command, []string) error { t.Fatal("served"); return nil }}
	configureServing(cmd, agent)
	cmd.SetArgs(args)
	return agent, cmd.Execute()
}

func TestDecisionModelDisabledByDefault(t *testing.T) {
	agent, _ := configuredAgent(t, "")
	if _, err := agent.Decide(t.Context(), bonnie.DecisionRequest{}); !errors.Is(err, runtime.ErrNoDecider) {
		t.Fatalf("decide without model: %v", err)
	}
}

func TestDecisionModelSelection(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		args      []string
	}{
		{"environment", "ollama/env-decider", nil},
		{"flag", "", []string{"--decision-model", "ollama/flag-decider"}},
		{"override", "ollama/env-decider", []string{"--decision-model=ollama/flag-decider"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, err := configuredAgent(t, tc.env, tc.args...)
			if err == nil || err.Error() != "jawa: NATS presence connection failed" {
				t.Fatalf("serving error: %v", err)
			}
			// A cancelled request reaches Kit only when a model is configured.
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := agent.Decide(ctx, bonnie.DecisionRequest{}); err == nil || errors.Is(err, runtime.ErrNoDecider) {
				t.Fatalf("decision model not configured: %v", err)
			}
		})
	}
}

func TestDecisionModelEmptyFlagDisables(t *testing.T) {
	agent, _ := configuredAgent(t, "ollama/env-decider", "--decision-model=")
	if _, err := agent.Decide(t.Context(), bonnie.DecisionRequest{}); !errors.Is(err, runtime.ErrNoDecider) {
		t.Fatalf("explicit empty flag: %v", err)
	}
}

func TestInvalidDecisionModelBeforeDial(t *testing.T) {
	for _, selection := range []string{"secret-selection", "secret/", "/secret"} {
		t.Run(selection, func(t *testing.T) {
			_, err := configuredAgent(t, "", "--decision-model", selection)
			if err == nil || err.Error() != "jawa: invalid decision model selection; expected provider/model" {
				t.Fatalf("unsafe or wrong error: %v", err)
			}
		})
	}
}

// TestLiveDecisionModel asks a real decision model through the serving
// configuration. JAWA_DECISION_MODEL overrides the default OpenRouter model.
func TestLiveDecisionModel(t *testing.T) {
	if os.Getenv("JAWA_DECISION_LIVE") != "1" {
		t.Skip("set JAWA_DECISION_LIVE=1 and the provider credential")
	}
	model := envOr("JAWA_DECISION_MODEL", liveDecisionModel)
	agent, err := configuredAgent(t, "", "--decision-model", model)
	if err == nil || err.Error() != "jawa: NATS presence connection failed" {
		t.Fatalf("serving error: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	res, err := agent.Decide(ctx, bonnie.DecisionRequest{
		State: kit.DecisionState{Text: "git push --force origin master"},
		Questions: []kit.DecisionQuestion{
			kit.BooleanQuestion("destructive", "Does this command delete or overwrite data?"),
		},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	a, err := res.Boolean("destructive")
	if err != nil {
		t.Fatal(err)
	}
	if a.Probability < 0.5 {
		t.Fatalf("P(destructive) = %v, want a clear yes", a.Probability)
	}
	t.Logf("%s: P(destructive) = %.3f", model, a.Probability)
}
