package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	client "github.com/mark3labs/bonnie/client/nats"
)

var branchSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// taskBranch assigns a bounded ASCII Git ref with a readable title and stable
// card/attempt identity. Once snapshotted in TaskJSON it must never be rewritten.
func taskBranch(title, cardID string, number int) string {
	var slug strings.Builder
	separator := false
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator && slug.Len() > 0 && slug.Len() < 40 {
				slug.WriteByte('-')
			}
			if slug.Len() >= 40 {
				break
			}
			slug.WriteRune(r)
			separator = false
		} else {
			separator = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		name = "task"
	}
	return fmt.Sprintf("jawa/%s-%s-a%d", name, branchCardID(cardID), number)
}

func branchCardID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

// assignedBranch reads the immutable trailing instruction and validates its
// identity. Legacy long refs remain valid for already queued/running tasks.
func assignedBranch(a Attempt) (string, error) {
	var task client.Task
	if json.Unmarshal([]byte(a.TaskJSON), &task) != nil || task.TaskID != a.TaskID || a.TaskID == "" || a.Number < 1 {
		return "", fmt.Errorf("invalid immutable task")
	}
	if strings.Count(task.Text, "\nBranch: ") != 1 {
		return "", fmt.Errorf("task lacks deterministic Branch instruction")
	}
	_, branch, _ := strings.Cut(task.Text, "\nBranch: ")
	legacy := fmt.Sprintf("jawa/card/%s/attempt/%d", a.CardID, a.Number)
	if branch == legacy {
		return branch, nil
	}
	suffix := fmt.Sprintf("-%s-a%d", branchCardID(a.CardID), a.Number)
	// Keep the previously emitted 12-character assignments valid.
	if len(a.CardID) >= 12 {
		previous := fmt.Sprintf("-%s-a%d", a.CardID[:12], a.Number)
		if strings.HasSuffix(branch, previous) {
			suffix = previous
		}
	}
	if !strings.HasPrefix(branch, "jawa/") || !strings.HasSuffix(branch, suffix) {
		return "", fmt.Errorf("task branch identity mismatch")
	}
	slug := strings.TrimSuffix(strings.TrimPrefix(branch, "jawa/"), suffix)
	if len(slug) > 40 || !branchSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("invalid task branch slug")
	}
	return branch, nil
}
