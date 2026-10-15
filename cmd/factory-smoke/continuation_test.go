package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Honor cancellation before producing the otherwise immediate fixture response.
type continuationModel struct{ model }

func (m continuationModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.model.Generate(ctx, call)
}

func (m continuationModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.model.Stream(ctx, call)
}

// Exercise BONNIE's real managed factory, not a replacement AgentFactory:
// WithCompletionHook adds the outer wrapper that must preserve ContinueResult.
// All journal writes are confined to a temporary directory; no broker or host
// server is started. The first turn stops after Kit durably saves its input.
func TestManagedCompletionContinuesSavedPrompt(t *testing.T) {
	const runID = "managed-continuation"
	const prompt = "offline continuation fixture"
	journal, work := filepath.Join(t.TempDir(), "journal"), t.TempDir()
	completionCalls := 0
	newAgent := func(interrupt func()) *bonnie.Agent {
		return bonnie.New(
			bonnie.WithJournal(journal), bonnie.WithInstructions(""),
			bonnie.WithContextFiles(""), bonnie.WithSkills(""), bonnie.WithoutHumanInput(),
			bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(work))),
			bonnie.WithKit(func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills = true, true, true
				o.NoExtensions, o.NoAgents, o.Quiet = true, true, true
				kit.WithProvider("smoke", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
					// Existing offline fixture returns immediate text for non-planning prompts.
					return &kit.ProviderResult{Model: continuationModel{model{planning: &planningScript{}}}}, nil
				})(o)
				kit.WithModel("smoke/final")(o)
			}),
			bonnie.WithKitSetup(func(_ context.Context, k *kit.Kit, _ bonnie.RunScope) error {
				if interrupt != nil {
					k.OnPrepareStep(kit.HookPriorityNormal, func(kit.PrepareStepHook) *kit.PrepareStepResult {
						interrupt()
						return nil
					})
				}
				return nil
			}),
			bonnie.WithCompletionHook(bonnie.CompletionPolicy{NewHook: func(context.Context, bonnie.RunScope) (bonnie.CompletionHook, error) {
				return func(context.Context, bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
					completionCalls++
					return bonnie.CompletionFeedback{}, nil
				}, nil
			}}),
		)
	}
	var runner *runtime.Runner
	interrupt := func() {
		if err := runner.Interrupt(runID); err != nil {
			t.Errorf("interrupt: %v", err)
		}
	}
	// Interrupt through the runner: durable turns deliberately ignore caller cancellation.
	err := newAgent(interrupt).WithRuntime(t.Context(), func(_ context.Context, rt *bonnie.Runtime) error {
		runner = rt.Runner()
		run, err := runner.Start(t.Context(), runID, runtime.Input{Text: prompt})
		if err != nil {
			return err
		}
		if run.State != runtime.RunInterrupted {
			return fmt.Errorf("first state = %s, want interrupted", run.State)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if completionCalls != 0 {
		t.Fatal("interrupted turn reached completion hook")
	}
	// A fresh host/factory must restore the saved prompt through the wrapper.
	err = newAgent(nil).WithRuntime(t.Context(), func(ctx context.Context, rt *bonnie.Runtime) error {
		run, err := rt.Runner().StartRetry(ctx, runID, runtime.Input{Text: prompt})
		if err != nil {
			return err
		}
		if run.State != runtime.RunCompleted {
			return fmt.Errorf("continued outcome = %+v, want completed", run)
		}
		records, err := rt.Journal().Replay(ctx, runID)
		if err != nil {
			return err
		}
		users := 0
		for _, record := range records {
			if record.Kind == runtime.RecordMessage && record.Role == "user" {
				users++
				if record.Text != prompt {
					return fmt.Errorf("unexpected user message %q", record.Text)
				}
			}
		}
		if users != 1 {
			return fmt.Errorf("user messages = %d, want exactly one saved prompt", users)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if completionCalls != 1 {
		t.Fatalf("completion calls = %d, want 1", completionCalls)
	}
}
