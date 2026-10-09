package orchestrator

import (
	"encoding/json"
	"fmt"
	client "github.com/mark3labs/bonnie/client/nats"
	"strings"
	"testing"
)

func TestTaskBranch(t *testing.T) {
	id := strings.Repeat("abcdef", 10)
	for _, tc := range []struct{ title, slug string }{
		{"Update CI", "update-ci"}, {" Fix / session_timeout... ", "fix-session-timeout"}, {"你好 🚀", "task"}, {"foo.lock @{bar}", "foo-lock-bar"}, {strings.Repeat("a", 100), strings.Repeat("a", 40)},
	} {
		got := taskBranch(tc.title, id, 2)
		want := "jawa/" + tc.slug + "-abcdef-a2"
		if got != want {
			t.Fatalf("%q: got %q want %q", tc.title, got, want)
		}
	}
}

func TestAssignedBranchCompatibility(t *testing.T) {
	id := strings.Repeat("a", 64)
	fresh := taskBranch("Update CI", id, 1)
	legacy := fmt.Sprintf("jawa/card/%s/attempt/1", id)
	for _, branch := range []string{fresh, legacy, "jawa/changed-slug-aaaaaaaaaaaa-a1", "jawa/changed-slug-aaaaaa-a1"} {
		a := branchAttempt(t, id, 1, "Implement\nBranch: "+branch)
		got, err := assignedBranch(a)
		if err != nil || got != branch {
			t.Fatalf("%q: %q %v", branch, got, err)
		}
		if attemptBranch(a) != branch {
			t.Fatal("UI changed persisted assignment")
		}
	}
	for _, text := range []string{
		"Implement", "Implement\nBranch: " + fresh + "\nextra", "Implement\nBranch: " + fresh + "\nBranch: " + fresh,
		"Implement\nBranch: jawa/update-ci-bbbbbbbbbbbb-a1", "Implement\nBranch: jawa/update-ci-aaaaaaaaaaaa-a2",
		"Implement\nBranch: jawa/../update-ci-aaaaaaaaaaaa-a1", "Implement\nBranch: jawa/-bad-aaaaaaaaaaaa-a1",
	} {
		if _, err := assignedBranch(branchAttempt(t, id, 1, text)); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func branchAttempt(t *testing.T, id string, n int, text string) Attempt {
	t.Helper()
	b, err := json.Marshal(client.Task{TaskID: "task", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return Attempt{CardID: id, Number: n, TaskID: "task", TaskJSON: string(b)}
}
