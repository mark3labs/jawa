package main

import (
	"context"
	"fmt"
	"unicode/utf8"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

const (
	maxCompletionContinuations = 2
	// Bounds what one ending check sends to the decision model.
	endingStateLimit = 16 << 10
)

// missingReport reports whether a run is finishing without a typed report.
type missingReport func(ctx context.Context, runID string) (bool, error)

// completionPolicy checks Go diagnostics first, then the typed report.
func completionPolicy(missing missingReport) bonnie.CompletionPolicy {
	return bonnie.CompletionPolicy{MaxContinuations: maxCompletionContinuations, NewHook: func(ctx context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
		baseline, err := snapshotGoFiles(ctx, scope.Exec)
		if err != nil {
			return nil, err
		}
		// Keep the initial baseline through repair turns; don't bless unfinished work.
		return func(ctx context.Context, c bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
			feedback, err := checkGoCompletion(ctx, scope.Exec, baseline)
			if err != nil || feedback.ContinueWith != "" {
				return feedback, err
			}
			return checkTypedReport(ctx, missing, scope.RunID, scope.Decide, c), nil
		}, nil
	}}
}

// Report prompts for each way a run can end without a typed report.
const (
	reportFinished   = "You described the work as finished, but you sent no typed report. Natural text does not complete a task. If the work is verified, call completeTask with the pull request number (a planning task calls proposeSpec instead). If anything is unverified, finish it first."
	reportBlocked    = "You described a blocker, but you sent no typed report. Natural text does not reach the operator. Call reportBlocker with the specific decision or access you need; it waits for guidance."
	reportQuestion   = "You asked a question in natural text, but nobody reads it. Planning tasks call clarify with the questions; build tasks call reportBlocker with the decision needed."
	reportUnfinished = "You stopped before the task was done, and you sent no typed report. Continue the work. When it is verified, call completeTask (a planning task calls proposeSpec); if you cannot continue, call reportBlocker."
	reportGeneric    = "You are ending this task without a typed report, and natural text does not complete it. Continue until you can call completeTask (a planning task calls proposeSpec), or call reportBlocker (planning may call clarify) if you cannot continue."
)

var reportEndings = map[string]string{
	"finished":   reportFinished,
	"blocked":    reportBlocked,
	"question":   reportQuestion,
	"unfinished": reportUnfinished,
}

// checkTypedReport sends a run that ends without a typed report back to the
// model with a prompt to report. The decision model, when configured, only
// picks the prompt: it never completes a task. A run that cannot continue or
// cannot be checked is accepted, and the orchestrator handles its missing report.
func checkTypedReport(ctx context.Context, missing missingReport, runID string, decide func(context.Context, bonnie.DecisionRequest) (*kit.DecisionResult, error), c bonnie.CompletionCandidate) bonnie.CompletionFeedback {
	if missing == nil || c.ContinuationsUsed >= maxCompletionContinuations {
		return bonnie.CompletionFeedback{}
	}
	if need, err := missing(ctx, runID); err != nil || !need {
		return bonnie.CompletionFeedback{}
	}
	return bonnie.CompletionFeedback{ContinueWith: reportPrompt(ctx, decide, c)}
}

func reportPrompt(ctx context.Context, decide func(context.Context, bonnie.DecisionRequest) (*kit.DecisionResult, error), c bonnie.CompletionCandidate) string {
	if decide == nil {
		return reportGeneric
	}
	// One check per continuation: a recovered check gets the journalled answer.
	res, err := decide(ctx, bonnie.DecisionRequest{
		State: kit.DecisionState{Text: endingState(c.Response)},
		Questions: []kit.DecisionQuestion{kit.ChoiceQuestion("ending", "This is the final message of a coding agent. How does it end?",
			kit.DecisionOption{Name: "finished", Description: "It says the work is complete."},
			kit.DecisionOption{Name: "blocked", Description: "It says it cannot proceed without access, a decision or a fix outside its control."},
			kit.DecisionOption{Name: "question", Description: "It asks the reader a question."},
			kit.DecisionOption{Name: "unfinished", Description: "It stops with work left to do, or describes a plan."},
		)},
		Key: fmt.Sprintf("typed-report:%d", c.ContinuationsUsed),
	})
	if err != nil {
		return reportGeneric
	}
	a, err := res.Choice("ending")
	if prompt, ok := reportEndings[a.Choice]; err == nil && !a.Refused && ok {
		return prompt
	}
	return reportGeneric
}

// endingState keeps the end of a long response, where its conclusion is.
func endingState(response string) string {
	if response == "" {
		return "(empty response)"
	}
	if len(response) <= endingStateLimit {
		return response
	}
	tail := response[len(response)-endingStateLimit:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail
}
