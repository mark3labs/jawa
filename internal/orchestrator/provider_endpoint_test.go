package orchestrator

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

func TestForgejoRepositoryContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		host, name string
		supported  bool
	}{
		{"https://forge.example", "team/repo", true},
		{"https://forge.example:3000/", "team/my_repo.git", true},
		{"https://[::1]:3000", "team/repo", true},
		{"http://localhost:3000", "team/repo", false},
		{"https://forge.example/git", "team/repo", false},
		{"https://forge.example/git/", "team/repo", false},
		{"https://user:pass@forge.example", "team/repo", false},
		{"https://forge.example", "team/repo?", false},
		{"https://forge.example", "team/repo?x=1", false},
		{"https://forge.example", "team/repo#fragment", false},
		{"https://forge.example", "team/%72epo", false},
		{"https://forge.example", "team/repo.git.git", true},
		{"https://forge.example", "team/.git", false},
		{"https://forge.example", "team/..", false},
		{"https://forge.example", "team/repo/extra", false},
	} {
		t.Run(tc.host+"/"+tc.name, func(t *testing.T) {
			repo := strings.TrimRight(tc.host, "/") + "/" + tc.name
			_, formErr := projectRepositoryURL("forgejo", tc.name, tc.host)
			_, _, _, parserErr := providerRepository(repo)
			if (formErr == nil) != tc.supported || validRepository(repo) != tc.supported || (parserErr == nil) != tc.supported {
				t.Fatalf("contract disagreement for %q: form=%v store=%v parser=%v, want supported=%v", repo, formErr, validRepository(repo), parserErr, tc.supported)
			}
		})
	}
	for _, repo := range []string{"ssh://git@forge.example:3000/team/repo.git", "git@forge.example:team/repo.git", "git://forge.example/team/repo"} {
		if !validRepository(repo) {
			t.Errorf("rejected repository identity %q", repo)
		}
	}
}

func TestUnsupportedForgejoProjectCreation(t *testing.T) {
	t.Parallel()
	a := testApp(t)
	session, csrf := setupTest(t, a)
	for _, repo := range []string{"http://forge.example/team/repo", "https://forge.example/git/team/repo"} {
		if _, err := a.s.CreateProjectDetails("unsupported", "forgejo", repo, "main"); err == nil || !strings.Contains(err.Error(), "root-path") {
			t.Fatalf("store did not clearly reject %q: %v", repo, err)
		}
		form := url.Values{"_csrf": {csrf.Value}, "name": {"unsupported"}, "provider": {"forgejo"}, "repo": {repo}}
		status(t, request(a, "POST", "/projects", form, session, csrf), 400)
	}
	for _, host := range []string{"http://forge.example", "https://forge.example/git"} {
		form := url.Values{"_csrf": {csrf.Value}, "name": {"unsupported"}, "provider": {"forgejo"}, "repo_name": {"team/repo"}, "forgejo_url": {host}}
		resp := request(a, "POST", "/projects", form, session, csrf)
		status(t, resp, 400)
		if !strings.Contains(resp.Body.String(), "unsupported") {
			t.Fatal("missing clear form error")
		}
	}
	projects, err := a.s.Projects()
	if err != nil || len(projects) != 0 {
		t.Fatalf("unsupported project persisted: %+v, %v", projects, err)
	}
}

func TestProviderEndpointCredentials(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			key := provider + "-token"
			env := strings.ToUpper(provider) + "_TOKEN"
			t.Setenv(env, "fallback")
			t.Setenv("JAWA_"+env, "")
			repo := "https://github.com/team/repo.git"
			api, prefix, kind := "https://api.github.com", "Bearer ", "pull"
			if provider == "forgejo" {
				repo = "https://forge.example:3000/team/repo.git"
				api, prefix, kind = "https://forge.example:3000/api/v1", "token ", "pulls"
			}
			for _, tc := range []struct {
				scoped   string
				settings map[string]string
				want     string
			}{
				{"", nil, "fallback"},
				{"scoped", nil, "scoped"},
				{"scoped", map[string]string{key: "dashboard"}, "dashboard"},
				{"scoped", map[string]string{key: ""}, ""},
			} {
				t.Setenv("JAWA_"+env, tc.scoped)
				e, err := resolveProviderEndpoint(provider, repo, tc.settings)
				if err != nil || e.Token != tc.want || e.API != api || e.AuthPrefix != prefix || e.PullKind != kind || e.Owner != "team" || e.Name != "repo" {
					t.Fatalf("unexpected endpoint coordinates or credentials (error=%v)", err)
				}
				if errors.Is(e.requireCredentials(), errMissingCredentials) != (tc.want == "") {
					t.Fatal("incorrect credential requirement")
				}
			}
		})
	}
	for _, tc := range []struct {
		provider, repo string
		want           error
	}{
		{"github", "https://forge.example/team/repo", errUnsupportedHost},
		{"other", "https://forge.example/team/repo", errUnsupportedProvider},
	} {
		_, err := resolveProviderEndpoint(tc.provider, tc.repo, nil)
		if !errors.Is(err, tc.want) {
			t.Fatalf("got %v, want %v", err, tc.want)
		}
	}
}
