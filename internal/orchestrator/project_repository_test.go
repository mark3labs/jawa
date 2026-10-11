package orchestrator

import (
	"net/url"
	"strings"
	"testing"
)

func TestProjectRepositoryURL(t *testing.T) {
	for _, tc := range []struct{ provider, name, host, want string }{
		{"github", " acme/repo ", "https://ignored.example", "https://github.com/acme/repo"},
		{"forgejo", "acme/repo", "https://codeberg.org", "https://codeberg.org/acme/repo"},
		{"forgejo", "team/my_repo.git", " https://forge.example:3000/git/ ", "https://forge.example:3000/git/team/my_repo.git"},
		{"forgejo", "team/repo", "http://localhost:3000", "http://localhost:3000/team/repo"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got, err := projectRepositoryURL(tc.provider, tc.name, tc.host)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, name := range []string{"", "repo", "/repo", "owner/", "a/b/c", "https://github.com/a/b", "a/..", "./b", "a/b?x", "a/b#x", "a/b c", "a/%2e%2e", "a/b\\c"} {
		if _, err := projectRepositoryURL("github", name, ""); err == nil {
			t.Errorf("accepted invalid repository name %q", name)
		}
	}
	for _, host := range []string{"", "forge.example", "ssh://forge.example", "https://", "https://user:pass@forge.example", "https://forge.example?x=1", "https://forge.example?", "https://forge.example#x", "https://forge.example/a/../b", "https://forge.example/%2f", "https://forge.example/has space"} {
		if _, err := projectRepositoryURL("forgejo", "a/b", host); err == nil {
			t.Errorf("accepted invalid Forgejo URL %q", host)
		}
	}
	if _, err := projectRepositoryURL("gitlab", "a/b", ""); err == nil {
		t.Error("accepted unsupported provider")
	}
}

func TestCreateProjectRepositoryNames(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	for _, tc := range []struct{ provider, name, host, want string }{
		{"github", "acme/github-repo", "", "https://github.com/acme/github-repo"},
		{"forgejo", "acme/forgejo-repo", "https://codeberg.org", "https://codeberg.org/acme/forgejo-repo"},
	} {
		form := url.Values{"_csrf": {csrf.Value}, "name": {tc.provider}, "provider": {tc.provider}, "repo_name": {tc.name}, "forgejo_url": {tc.host}, "base_branch": {"main"}}
		status(t, request(a, "POST", "/projects", form, session, csrf), 303)
		projects, err := a.s.Projects()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range projects {
			if p.Name == tc.provider {
				found = p.Repo == tc.want && p.Provider == tc.provider && p.BaseBranch == "main"
			}
		}
		if !found {
			t.Fatalf("constructed repository missing: %+v", projects)
		}
	}
	form := url.Values{"_csrf": {csrf.Value}, "name": {"invalid"}, "provider": {"github"}, "repo_name": {""}, "repo": {"https://github.com/a/b"}}
	status(t, request(a, "POST", "/projects", form, session, csrf), 400)
	projects, err := a.s.Projects()
	if err != nil || len(projects) != 2 {
		t.Fatalf("invalid form persisted: %+v, %v", projects, err)
	}
	body := request(a, "GET", "/board", nil, session, csrf).Body.String()
	for _, fragment := range []string{`name="repo_name"`, `placeholder="owner/repo"`, `name="forgejo_url"`, `value="https://codeberg.org"`, `data-bind:project-provider`, `data-attr:hidden`, `data-attr:disabled`} {
		if !strings.Contains(body, fragment) {
			t.Errorf("missing form fragment %s", fragment)
		}
	}
}
