package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/sandbox"
)

const (
	diagnosticsLimit   = 12000
	diagnosticsTimeout = 2 * time.Minute
	goplsTimeout       = 10 * time.Minute
)

// Paths and hashes are separate NUL-terminated fields, so whitespace and shell
// metacharacters in filenames remain data. All file access happens via Exec.
const goSnapshot = `set -euo pipefail
find . -type d \( -name .git -o -name vendor -o -name .cache -o -name node_modules -o -path './go/pkg' \) -prune -o -type f -name '*.go' -print0 |
while IFS= read -r -d '' file; do
 hash=$(sha256sum < "$file")
 printf '%s\0%s\0' "$file" "${hash%% *}"
done`

func snapshotGoFiles(ctx context.Context, exec sandboxExec) (map[string]string, error) {
	res, err := exec(ctx, sandbox.Command{Args: []string{"bash", "-c", goSnapshot}, Timeout: diagnosticsTimeout})
	if err != nil {
		return nil, err
	}
	if res == nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("snapshot Go files: %s", diagnosticOutput(res, nil))
	}
	return parseGoSnapshot(res.Stdout)
}

func parseGoSnapshot(output string) (map[string]string, error) {
	files := make(map[string]string)
	if output == "" {
		return files, nil
	}
	fields := strings.Split(output, "\x00")
	if fields[len(fields)-1] != "" || len(fields)%2 != 1 {
		return nil, fmt.Errorf("invalid Go file snapshot")
	}
	for i := 0; i < len(fields)-1; i += 2 {
		files[fields[i]] = fields[i+1]
	}
	return files, nil
}

// For newly appearing files, a clean Git checkout is a clone, not an edit.
// Resolve the enclosing module in the run environment as well.
const changedPackage = `set -euo pipefail
file=$1
existing=$2
dir=$(dirname "$file")
if [ "$existing" = new ] && git -C "$dir" rev-parse --show-toplevel >/dev/null 2>&1; then
 status=$(git -C "$dir" status --porcelain -- "$(basename "$file")")
 if [ -z "$status" ]; then exit 0; fi
fi
root=$dir
while [ ! -f "$root/go.mod" ]; do
 if [ "$root" = . ] || [ "$root" = / ]; then exit 0; fi
 root=$(dirname "$root")
done
printf '%s\0%s\0' "$root" "$dir"`

func changedGoPackages(ctx context.Context, exec sandboxExec, baseline map[string]string) (map[string][]string, error) {
	current, err := snapshotGoFiles(ctx, exec)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]bool)
	for path, hash := range current {
		if old, ok := baseline[path]; !ok || old != hash {
			paths[path] = true
		}
	}
	for path := range baseline {
		if _, ok := current[path]; !ok {
			paths[path] = true
		}
	}
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	sets := make(map[string]map[string]bool)
	for _, path := range ordered {
		kind := "new"
		if _, ok := baseline[path]; ok {
			kind = "existing"
		}
		res, err := exec(ctx, sandbox.Command{Args: []string{"bash", "-c", changedPackage, "go-diagnostics", path, kind}, Timeout: diagnosticsTimeout})
		if err != nil {
			return nil, err
		}
		if res == nil || res.ExitCode != 0 {
			return nil, fmt.Errorf("resolve changed package: %s", diagnosticOutput(res, nil))
		}
		if res.Stdout == "" {
			continue
		}
		fields := strings.Split(res.Stdout, "\x00")
		if len(fields) != 3 || fields[2] != "" {
			return nil, fmt.Errorf("invalid changed package result")
		}
		rel, err := filepath.Rel(fields[0], fields[1])
		if err != nil {
			return nil, err
		}
		if sets[fields[0]] == nil {
			sets[fields[0]] = make(map[string]bool)
		}
		sets[fields[0]]["./"+filepath.ToSlash(rel)] = true
	}
	packages := make(map[string][]string)
	for root, dirs := range sets {
		for dir := range dirs {
			packages[root] = append(packages[root], dir)
		}
		sort.Strings(packages[root])
	}
	return packages, nil
}

type sandboxExec func(context.Context, sandbox.Command) (*sandbox.Result, error)

func goCompletionPolicy() bonnie.CompletionPolicy {
	return bonnie.CompletionPolicy{MaxContinuations: 2, NewHook: func(ctx context.Context, scope bonnie.RunScope) (bonnie.CompletionHook, error) {
		baseline, err := snapshotGoFiles(ctx, scope.Exec)
		if err != nil {
			return nil, err
		}
		// Keep the initial baseline through repair turns; don't bless unfinished work.
		return func(ctx context.Context, _ bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
			return checkGoCompletion(ctx, scope.Exec, baseline)
		}, nil
	}}
}

func checkGoCompletion(ctx context.Context, exec sandboxExec, baseline map[string]string) (bonnie.CompletionFeedback, error) {
	packages, err := changedGoPackages(ctx, exec, baseline)
	if err != nil {
		return bonnie.CompletionFeedback{}, err
	}
	return checkGoPackages(ctx, exec, packages)
}

func checkGoPackages(ctx context.Context, exec sandboxExec, packages map[string][]string) (bonnie.CompletionFeedback, error) {
	var sections []string
	dirs := make([]string, 0, len(packages))
	for dir := range packages {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		targets := packages[dir]
		if len(targets) == 0 {
			continue
		}
		// go list selects files for the current build configuration. Ignored scripts
		// and alternate-platform files are not fed directly to gopls.
		for _, check := range []struct {
			name    string
			args    []string
			timeout time.Duration
		}{
			{"gopls", append([]string{"bash", "-c", `set -euo pipefail
for pkg in "$@"; do
 files=$(go list -f '{{range .GoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .CgoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .TestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' "$pkg")
 if [ -n "$files" ]; then printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 -r gopls check; fi
done`, "go-diagnostics"}, targets...), goplsTimeout},
			{"golangci-lint", append([]string{"golangci-lint", "run", "--timeout=2m", "--show-stats=false", "--output.text.path", "stdout", "--output.text.colors=false", "--output.text.print-issued-lines=false"}, targets...), diagnosticsTimeout},
		} {
			res, runErr := exec(ctx, sandbox.Command{Args: check.args, Dir: dir, Timeout: check.timeout})
			if ctx.Err() != nil {
				return bonnie.CompletionFeedback{}, ctx.Err()
			}
			if output := diagnosticOutput(res, runErr); output != "" {
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
	return bonnie.CompletionFeedback{ContinueWith: "<go_diagnostics>\n" + report + "\n</go_diagnostics>\nReview diagnostics in packages changed during this execution. Fix only issues introduced by your authorized edits. Do not modify unrelated code, ignored examples, or unsupported platforms. If findings are pre-existing or validation is unavailable, report the limitation instead of inventing fixes. Summarize validation."}, nil
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
