package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func scriptedEnding(choice string, refused bool, err error, seen *bonnie.DecisionRequest) func(context.Context, bonnie.DecisionRequest) (*kit.DecisionResult, error) {
	return func(_ context.Context, req bonnie.DecisionRequest) (*kit.DecisionResult, error) {
		*seen = req
		if err != nil {
			return nil, err
		}
		return &kit.DecisionResult{Answers: []kit.DecisionAnswer{{ID: "ending", Type: kit.DecisionChoice, Choice: choice, Refused: refused}}}, nil
	}
}

func TestCheckTypedReport(t *testing.T) {
	ctx := t.Context()
	missing := func(want bool, err error) missingReport {
		return func(_ context.Context, runID string) (bool, error) {
			if runID != "run" {
				t.Fatalf("run = %q", runID)
			}
			return want, err
		}
	}
	candidate := bonnie.CompletionCandidate{Response: "All done, PR #7 is open.", ContinuationsUsed: 1}
	for _, tc := range []struct {
		name    string
		missing missingReport
		choice  string
		refused bool
		err     error
		want    string
	}{
		{"reported", missing(false, nil), "finished", false, nil, ""},
		{"journal error", missing(false, errors.New("journal")), "finished", false, nil, ""},
		{"finished", missing(true, nil), "finished", false, nil, reportFinished},
		{"blocked", missing(true, nil), "blocked", false, nil, reportBlocked},
		{"question", missing(true, nil), "question", false, nil, reportQuestion},
		{"unfinished", missing(true, nil), "unfinished", false, nil, reportUnfinished},
		{"unknown choice", missing(true, nil), "other", false, nil, reportGeneric},
		{"refused", missing(true, nil), "finished", true, nil, reportGeneric},
		{"decision error", missing(true, nil), "", false, errors.New("provider"), reportGeneric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen bonnie.DecisionRequest
			got := checkTypedReport(ctx, tc.missing, "run", scriptedEnding(tc.choice, tc.refused, tc.err, &seen), candidate)
			if got.ContinueWith != tc.want {
				t.Fatalf("feedback = %q, want %q", got.ContinueWith, tc.want)
			}
			if tc.want != "" && (seen.Key != "typed-report:1" || seen.State.Text != candidate.Response || len(seen.Questions) != 1 || seen.Questions[0].ID != "ending") {
				t.Fatalf("decision request = %+v", seen)
			}
		})
	}
	t.Run("no decision model", func(t *testing.T) {
		if got := checkTypedReport(ctx, missing(true, nil), "run", nil, candidate); got.ContinueWith != reportGeneric {
			t.Fatalf("feedback = %q", got.ContinueWith)
		}
	})
	t.Run("continuation budget spent", func(t *testing.T) {
		spent := candidate
		spent.ContinuationsUsed = maxCompletionContinuations
		called := false
		probe := func(context.Context, string) (bool, error) { called = true; return true, nil }
		if got := checkTypedReport(ctx, probe, "run", nil, spent); got.ContinueWith != "" || called {
			t.Fatalf("spent budget continued: %q, checked %v", got.ContinueWith, called)
		}
	})
	t.Run("no bridge", func(t *testing.T) {
		if got := checkTypedReport(ctx, nil, "run", nil, candidate); got.ContinueWith != "" {
			t.Fatalf("feedback = %q", got.ContinueWith)
		}
	})
}

func TestEndingState(t *testing.T) {
	if got := endingState(""); got != "(empty response)" {
		t.Fatalf("empty = %q", got)
	}
	long := strings.Repeat("é", endingStateLimit) + "conclusion"
	got := endingState(long)
	if len(got) > endingStateLimit || !utf8.ValidString(got) || !strings.HasSuffix(got, "conclusion") {
		t.Fatalf("tail len %d valid %v", len(got), utf8.ValidString(got))
	}
}
