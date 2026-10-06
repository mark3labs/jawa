package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/sandbox"
)

const diagnosticsLimit = 12000

// Scan at completion rather than keeping an in-memory edit list: this includes
// shell edits and recovered/resumed runs. Exclude dependency and tool caches.
const moduleScan = `find . -type d \( -name .git -o -name vendor -o -name .cache -o -path './go/pkg' \) -prune -o -type f -name go.mod -print`

func goCompletionPolicy() bonnie.CompletionPolicy {
	return bonnie.CompletionPolicy{MaxContinuations: 2, NewHook: func(_ context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
		return func(ctx context.Context, _ bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
			return checkGoCompletion(ctx, scope.Exec)
		}, nil
	}}
}

type sandboxExec func(context.Context, sandbox.Command) (*sandbox.Result, error)

func checkGoCompletion(ctx context.Context, exec sandboxExec) (bonnie.CompletionFeedback, error) {
	scan, err := exec(ctx, sandbox.Command{Args: []string{"sh", "-c", moduleScan}, Timeout: 20 * time.Second})
	if err != nil {
		return bonnie.CompletionFeedback{}, fmt.Errorf("scan Go modules: %w", err)
	}
	if scan == nil || scan.ExitCode != 0 {
		return bonnie.CompletionFeedback{}, fmt.Errorf("scan Go modules failed: %v", scan)
	}
	var sections []string
	for module := range strings.SplitSeq(strings.TrimSpace(scan.Stdout), "\n") {
		if module == "" {
			continue
		}
		// find emits ./-prefixed paths, so names cannot be interpreted as flags.
		dir := strings.TrimSuffix(module, "/go.mod")
		for _, check := range []struct {
			name string
			args []string
		}{
			{"go fix", []string{"go", "fix", "./..."}},
			// Prune nested modules: they get their own independent checks below.
			{"gopls", []string{"sh", "-c", `find . -type d \( -name .git -o -name vendor -o -name .cache -o -path './go/pkg' \) -prune -o -type d ! -path . -exec test -f '{}/go.mod' \; -prune -o -type f -name '*.go' -print0 | xargs -0 -r gopls check`}},
			{"golangci-lint", []string{"golangci-lint", "run", "./...", "--timeout=20s", "--show-stats=false", "--output.text.path", "stdout", "--output.text.colors=false", "--output.text.print-issued-lines=false"}},
		} {
			if err := ctx.Err(); err != nil {
				return bonnie.CompletionFeedback{}, err
			}
			res, runErr := exec(ctx, sandbox.Command{Args: check.args, Dir: dir, Timeout: 20 * time.Second})
			if ctx.Err() != nil {
				return bonnie.CompletionFeedback{}, ctx.Err()
			}
			output := diagnosticOutput(res, runErr)
			if output != "" {
				sections = append(sections, fmt.Sprintf("[%s: %s]\n%s", dir, check.name, output))
			}
		}
	}
	if len(sections) == 0 {
		return bonnie.CompletionFeedback{}, nil
	}
	report := strings.Join(sections, "\n\n")
	if len(report) > diagnosticsLimit {
		report = report[:diagnosticsLimit] + "\n... output truncated ..."
	}
	return bonnie.CompletionFeedback{ContinueWith: "<go_diagnostics>\n" + report + "\n</go_diagnostics>\nReview and fix these diagnostics before finishing. go fix may have rewritten source: inspect the changes. Missing tools or timeouts are not clean checks. Summarize validation in your final response."}, nil
}

func diagnosticOutput(res *sandbox.Result, err error) string {
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if res == nil {
		return "ERROR: missing command result"
	}
	output := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
	if res.ExitCode != 0 {
		output = fmt.Sprintf("exit %d\n%s", res.ExitCode, output)
	}
	return output
}
