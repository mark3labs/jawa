package orchestrator

import "testing"

func TestProjectPRURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		provider, repo string
		number         int
		want           string
	}{
		{"github", "https://github.com/example/repo.git", 123, "https://github.com/example/repo/pull/123"},
		{"github", "git@github.com:example/repo.git", 7, "https://github.com/example/repo/pull/7"},
		{"forgejo", "ssh://git@forge.example:3000/team/repo.git", 42, "https://forge.example:3000/team/repo/pulls/42"},
		{"forgejo", "https://forge.example/team/repo", 1, "https://forge.example/team/repo/pulls/1"},
		{"github", "https://evil.example/team/repo", 1, ""},
		{"other", "https://forge.example/team/repo", 1, ""},
		{"github", "https://github.com/team/repo?redirect=evil", 1, ""},
		{"github", "https://github.com/team/repo", 0, ""},
		{"github", "https://github.com/team/repo", -1, ""},
	} {
		got, err := projectPRURL(tc.provider, tc.repo, tc.number)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Errorf("projectPRURL(%q, %q, %d) = %q, %v; want %q", tc.provider, tc.repo, tc.number, got, err, tc.want)
		}
	}
}
