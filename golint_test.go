package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/sandbox"
)

func TestCompletionChecksModules(t *testing.T) {
	var commands []sandbox.Command
	exec := func(_ context.Context, c sandbox.Command) (*sandbox.Result, error) {
		commands = append(commands, c)
		if len(commands) == 1 {
			return &sandbox.Result{Stdout: "./repo/go.mod\n./repo/nested/go.mod\n"}, nil
		}
		return &sandbox.Result{}, nil
	}
	hook, err := goCompletionPolicy().NewHook(context.Background(), bonnie.RunScope{Exec: exec})
	if err != nil {
		t.Fatal(err)
	}
	feedback, err := hook(context.Background(), bonnie.CompletionCandidate{})
	if err != nil || feedback.ContinueWith != "" {
		t.Fatalf("%+v %v", feedback, err)
	}
	if len(commands) != 7 {
		t.Fatalf("got %d commands", len(commands))
	}
	for i, c := range commands[1:] {
		want := "./repo"
		if i >= 3 {
			want = "./repo/nested"
		}
		if c.Dir != want || c.Timeout != 20*time.Second {
			t.Fatalf("unexpected command %+v", c)
		}
	}
	if goCompletionPolicy().MaxContinuations != 2 {
		t.Fatal("repair loop must be bounded")
	}
}
func TestCompletionNoGoModules(t *testing.T) {
	calls := 0
	feedback, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		calls++
		return &sandbox.Result{}, nil
	})
	if calls != 1 || err != nil || feedback.ContinueWith != "" {
		t.Fatalf("%d %+v %v", calls, feedback, err)
	}
}
func TestCompletionFeedback(t *testing.T) {
	calls := 0
	feedback, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		calls++
		if calls == 1 {
			return &sandbox.Result{Stdout: "./go.mod"}, nil
		}
		return &sandbox.Result{ExitCode: 1, Stderr: strings.Repeat("x", diagnosticsLimit+1)}, nil
	})
	if err != nil || !strings.Contains(feedback.ContinueWith, "exit 1") || !strings.Contains(feedback.ContinueWith, "truncated") {
		t.Fatalf("%+v %v", feedback, err)
	}
}
func TestCompletionScanFailure(t *testing.T) {
	_, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		return nil, errors.New("sandbox unavailable")
	})
	if err == nil {
		t.Fatal("scan failures must not accept completion")
	}
}
func TestCompletionMissingTools(t *testing.T) {
	calls := 0
	feedback, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		calls++
		if calls == 1 {
			return &sandbox.Result{Stdout: "./go.mod"}, nil
		}
		return nil, errors.New("missing executable")
	})
	if err != nil || !strings.Contains(feedback.ContinueWith, "missing executable") {
		t.Fatalf("%+v %v", feedback, err)
	}
}
func TestCleanupRetention(t *testing.T) {
	p := workspaceCleanupPolicy()
	if p.CompletedAfter != 7*24*time.Hour || p.FailedAfter != 14*24*time.Hour || p.CancelledAfter != 3*24*time.Hour || p.RetiredAfter != 3*24*time.Hour {
		t.Fatalf("unexpected policy %+v", p)
	}
}
