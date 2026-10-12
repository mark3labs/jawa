package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
)

func conflictFixture(t *testing.T, provider string) (*Store, Attempt) {
	t.Helper()
	t.Setenv("JAWA_GITHUB_TOKEN", "test-token")
	t.Setenv("JAWA_FORGEJO_TOKEN", "test-token")
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	host, kind := "github.com", "pull"
	if provider == "forgejo" {
		host, kind = "forge.example", "pulls"
	}
	repo := "https://" + host + "/acme/repo.git"
	p, err := s.CreateProjectDetails("test", provider, repo, "main")
	if err != nil {
		t.Fatal(err)
	}
	card, err := s.CreateCard(p.ID, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	branch := fmt.Sprintf("jawa/card/%s/attempt/1", card.ID)
	task := client.Task{Version: 1, TaskID: "attempt", Text: "Implement\nRepository: " + repo + "\nBase branch: main\nProvider: " + provider + "\nIssue: \nBranch: " + branch}
	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return s, Attempt{ID: "attempt", CardID: card.ID, Number: 1, TaskID: "attempt", TaskJSON: string(data), PRURL: "https://" + host + "/acme/repo/" + kind + "/7", State: "ready", Ready: true}
}

func TestProviderConflictChecker(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, scenario := range []string{"conflict", "clear", "unknown", "blocked", "wrong repo", "wrong base", "wrong branch", "changed head", "changed base", "clear changed head", "clear changed base", "missing base SHA", "invalid base SHA", "clear missing base SHA", "clear became unknown", "cleared", "merged", "merged deleted repo", "merged missing base SHA", "merged changed head", "merged became open", "clear became merged", "conflict became merged", "open deleted repo", "merged wrong repo", "merged wrong number", "merged wrong SHA", "merged wrong base repo", "closed", "draft", "HTTP error", "malformed", "cancelled", "bad URL"} {
			t.Run(provider+"/"+scenario, func(t *testing.T) {
				s, a := conflictFixture(t, provider)
				branch, err := assignedBranch(a)
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				transport := providerTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Context().Err() != nil {
						return nil, r.Context().Err()
					}
					if strings.TrimPrefix(r.URL.Path, "/api/v1") != "/repos/acme/repo/pulls/7" {
						t.Errorf("unexpected request: %s", r.URL.Path)
					}
					ref, repo, base, sha := branch, "acme/repo", "main", strings.Repeat("a", 40)
					baseSHA := strings.Repeat("c", 40)
					mergeable, state, merged, dirty, draft := any(false), "open", false, "dirty", false
					switch scenario {
					case "clear", "clear changed head", "clear changed base", "clear missing base SHA", "clear became unknown":
						mergeable = true
					case "unknown":
						mergeable = nil
					case "blocked":
						dirty = "blocked"
					case "wrong repo":
						repo = "other/repo"
					case "wrong base":
						base = "other"
					case "wrong branch":
						ref = "other"
					case "missing base SHA":
						baseSHA = ""
					case "invalid base SHA":
						baseSHA = "not-a-sha"
					case "changed base":
						if calls == 2 {
							baseSHA = strings.Repeat("d", 40)
						}
					case "changed head":
						if calls == 2 {
							sha = strings.Repeat("b", 40)
						}
					case "cleared":
						if calls == 2 {
							mergeable = true
						}
					case "merged", "merged deleted repo", "merged missing base SHA", "merged changed head", "merged became open", "merged wrong repo", "merged wrong number", "merged wrong SHA", "merged wrong base repo":
						state, merged = "closed", true
						switch scenario {
						case "merged deleted repo":
							repo = ""
						case "merged missing base SHA":
							baseSHA = ""
						case "merged changed head":
							if calls == 2 {
								sha = strings.Repeat("b", 40)
							}
						case "merged became open":
							if calls == 2 {
								state, merged = "open", false
							}
						case "merged wrong repo":
							repo = "other/repo"
						case "merged wrong SHA":
							sha = "invalid"
						}
					case "clear became merged", "conflict became merged":
						mergeable = scenario == "clear became merged"
						if calls == 2 {
							state, merged, baseSHA, repo = "closed", true, "", ""
						}
					case "open deleted repo":
						repo = ""
					case "closed":
						state = "closed"
					case "draft":
						draft = true
					}
					if scenario == "clear changed head" && calls == 2 {
						sha = strings.Repeat("b", 40)
					}
					if scenario == "clear changed base" && calls == 2 {
						baseSHA = strings.Repeat("d", 40)
					}
					if scenario == "clear missing base SHA" {
						baseSHA = ""
					}
					if scenario == "clear became unknown" && calls == 2 {
						mergeable = nil
					}
					number, baseRepo := 7, "acme/repo"
					if scenario == "merged wrong number" {
						number = 8
					}
					if scenario == "merged wrong base repo" {
						baseRepo = "other/repo"
					}
					var headRepo any = map[string]any{"full_name": repo}
					if scenario == "merged deleted repo" {
						headRepo = nil
					}
					body, _ := json.Marshal(map[string]any{"number": number, "state": state, "merged": merged, "draft": draft, "mergeable": mergeable, "mergeable_state": dirty, "head": map[string]any{"ref": ref, "sha": sha, "repo": headRepo}, "base": map[string]any{"ref": base, "sha": baseSHA, "repo": map[string]any{"full_name": baseRepo}}})
					status := 200
					if scenario == "HTTP error" {
						status = 403
					}
					if scenario == "malformed" {
						body = []byte("{")
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				})
				ctx := context.Background()
				if scenario == "cancelled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				if scenario == "bad URL" {
					a.PRURL = "https://evil.example/acme/repo/pull/7"
				}
				check, err := ProviderConflictChecker(s, &http.Client{Transport: transport})(ctx, a)
				want := "unknown"
				switch scenario {
				case "conflict":
					want = "conflict"
				case "clear":
					want = "clear"
				case "blocked":
					if provider == "forgejo" {
						want = "conflict"
					}
				case "merged", "merged deleted repo", "merged missing base SHA", "clear became merged", "conflict became merged":
					want = "merged"
				case "closed":
					want = "closed"
				}
				wantError := strings.HasPrefix(scenario, "merged wrong ") || scenario == "open deleted repo" || strings.HasPrefix(scenario, "wrong ") || scenario == "HTTP error" || scenario == "malformed" || scenario == "cancelled" || scenario == "bad URL"
				if (err != nil) != wantError {
					t.Fatalf("error=%v wantError=%v", err, wantError)
				}
				if !wantError && check.State != want {
					t.Fatalf("state=%s want=%s", check.State, want)
				}
				if calls > 2 {
					t.Fatalf("unbounded calls: %d", calls)
				}
				if (want == "conflict" || want == "clear" || want == "merged") && calls != 2 {
					t.Fatalf("conflict not confirmed: %d", calls)
				}
				if scenario == "bad URL" && calls != 0 {
					t.Fatal("unsafe request")
				}
			})
		}
	}
}

func TestProviderVerifierRepairAssignedBranch(t *testing.T) {
	s, a := conflictFixture(t, "github")
	branch, err := assignedBranch(a)
	if err != nil {
		t.Fatal(err)
	}
	a.Number, a.Branch, a.RepairPRURL, a.RepairSourceNumber = 2, branch, a.PRURL, 1
	calls := 0
	transport := providerTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := fmt.Sprintf(`{"number":7,"state":"closed","merged":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","repo":{"full_name":"acme/repo"}}}`, branch, strings.Repeat("a", 40))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	ok, err := ProviderVerifier(s, &http.Client{Transport: transport})(context.Background(), a)
	if err != nil || !ok || calls != 2 {
		t.Fatalf("ok=%v err=%v calls=%d", ok, err, calls)
	}
}
