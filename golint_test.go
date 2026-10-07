package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/sandbox"
)

func TestParseGoSnapshot(t *testing.T) {
	files, err := parseGoSnapshot("./space name.go\x00hash\x00")
	if err != nil || files["./space name.go"] != "hash" {
		t.Fatalf("%v %v", files, err)
	}
	if _, err := parseGoSnapshot("truncated"); err == nil {
		t.Fatal("expected malformed output error")
	}
}
func TestCleanCloneDoesNotTriggerChecks(t *testing.T) {
	calls := 0
	feedback, err := checkGoCompletion(context.Background(), func(_ context.Context, c sandbox.Command) (*sandbox.Result, error) {
		calls++
		if calls == 1 {
			return &sandbox.Result{Stdout: "./repo/a.go\x00hash\x00"}, nil
		}
		if c.Args[4] != "./repo/a.go" || c.Args[5] != "new" {
			t.Fatalf("unexpected resolution %+v", c)
		}
		return &sandbox.Result{}, nil // Git reports clean cloned file.
	}, map[string]string{})
	if err != nil || feedback.ContinueWith != "" || calls != 2 {
		t.Fatalf("%+v %v calls=%d", feedback, err, calls)
	}
}
func TestChangedFilesDeduplicatePackages(t *testing.T) {
	calls := 0
	packages, err := changedGoPackages(context.Background(), func(_ context.Context, c sandbox.Command) (*sandbox.Result, error) {
		calls++
		if calls == 1 {
			return &sandbox.Result{Stdout: "./repo/a.go\x00new\x00./repo/b.go\x00new\x00"}, nil
		}
		return &sandbox.Result{Stdout: "./repo\x00./repo\x00"}, nil
	}, map[string]string{"./repo/a.go": "old", "./repo/b.go": "old"})
	if err != nil || len(packages["./repo"]) != 1 || packages["./repo"][0] != "./." {
		t.Fatalf("%v %v", packages, err)
	}
}
func TestUnchangedWorkspace(t *testing.T) {
	calls := 0
	feedback, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		calls++
		return &sandbox.Result{Stdout: "./a.go\x00hash\x00"}, nil
	}, map[string]string{"./a.go": "hash"})
	if err != nil || feedback.ContinueWith != "" || calls != 1 {
		t.Fatalf("%+v %v calls=%d", feedback, err, calls)
	}
}
func TestChecksAreScopedAndNonMutating(t *testing.T) {
	var commands []sandbox.Command
	feedback, err := checkGoPackages(context.Background(), func(_ context.Context, c sandbox.Command) (*sandbox.Result, error) {
		commands = append(commands, c)
		return &sandbox.Result{}, nil
	}, map[string][]string{"./repo": {"./pkg"}})
	if err != nil || feedback.ContinueWith != "" || len(commands) != 2 {
		t.Fatalf("%+v %v", feedback, err)
	}
	if commands[0].Timeout != goplsTimeout || !strings.Contains(commands[0].Args[2], "go list") || commands[1].Timeout != diagnosticsTimeout {
		t.Fatal("unexpected checks")
	}
	for _, c := range commands {
		if c.Dir != "./repo" || c.Args[len(c.Args)-1] != "./pkg" || strings.Contains(strings.Join(c.Args, " "), "go fix") {
			t.Fatalf("unexpected command %+v", c)
		}
	}
}
func TestCompletionFeedback(t *testing.T) {
	feedback, err := checkGoPackages(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) {
		return &sandbox.Result{ExitCode: 1, Stderr: strings.Repeat("x", diagnosticsLimit+1)}, nil
	}, map[string][]string{".": {"./pkg"}})
	if err != nil || !strings.Contains(feedback.ContinueWith, "exit 1") || !strings.Contains(feedback.ContinueWith, "truncated") {
		t.Fatalf("%+v %v", feedback, err)
	}
}
func TestCompletionScanFailure(t *testing.T) {
	_, err := checkGoCompletion(context.Background(), func(context.Context, sandbox.Command) (*sandbox.Result, error) { return nil, errors.New("unavailable") }, nil)
	if err == nil {
		t.Fatal("must not accept failed scan")
	}
}
func TestCleanupRetention(t *testing.T) {
	p := workspaceCleanupPolicy()
	if p.CompletedAfter != 7*24*time.Hour || p.FailedAfter != 14*24*time.Hour || p.CancelledAfter != 3*24*time.Hour || p.RetiredAfter != 3*24*time.Hour {
		t.Fatalf("unexpected policy %+v", p)
	}
	if goCompletionPolicy().MaxContinuations != 2 {
		t.Fatal("repair budget changed")
	}
}
