package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
)

type providerTransport func(*http.Request) (*http.Response, error)

func (f providerTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderVerifier(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, scenario := range []string{"success", "wrong repo", "wrong branch", "wrong head", "pending", "failed", "missing required", "red thread", "changes requested", "dismissed", "no CI", "allow no CI", "missing token", "bad URL", "legacy task", "graphql error", "changed head", "redirect", "cancelled", "malformed", "missing mergeability", "empty policy", "missing contexts", "missing checks", "empty rules", "rules empty parameters", "rules unavailable", "rules only", "rules missing required", "rules success", "optional failure", "pending review"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("JAWA_GITHUB_TOKEN", "test-secret")
			t.Setenv("GITHUB_TOKEN", "")
			t.Setenv("JAWA_ALLOW_NO_CI", "")
			if scenario == "missing token" {
				t.Setenv("JAWA_GITHUB_TOKEN", "")
			}
			if scenario == "allow no CI" {
				t.Setenv("JAWA_ALLOW_NO_CI", "true")
			}
			s, err := OpenStore(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			project, err := s.CreateProjectDetails("test", "github", "https://github.com/acme/repo.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			card, err := s.CreateCard(project.ID, "title", "description")
			if err != nil {
				t.Fatal(err)
			}
			branch := fmt.Sprintf("jawa/card/%s/attempt/1", card.ID)
			task := client.Task{Version: 1, TaskID: "task", Text: "Implement\nRepository: https://github.com/acme/repo.git\nBase branch: main\nProvider: github\nIssue: \nBranch: " + branch}
			if scenario == "legacy task" {
				task.Text = strings.TrimSuffix(task.Text, "\nBranch: "+branch)
			}
			data, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			a := Attempt{CardID: card.ID, Number: 1, TaskID: "task", TaskJSON: string(data), PRURL: "https://github.com/acme/repo/pull/7"}
			if scenario == "bad URL" {
				a.PRURL = "https://evil.example/acme/repo/pull/7"
			}
			calls, prCalls := 0, 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("incorrect auth")
				}
				w.Header().Set("Content-Type", "application/json")
				if scenario == "redirect" {
					w.Header().Set("Location", "https://evil.example/secret")
					w.WriteHeader(302)
					return
				}
				if scenario == "malformed" {
					if _, err := fmt.Fprint(w, "{"); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
					return
				}
				switch {
				case r.URL.Path == "/repos/acme/repo/pulls/7":
					prCalls++
					repo, ref, head := "acme/repo", branch, sha
					if scenario == "wrong repo" {
						repo = "other/repo"
					}
					if scenario == "wrong branch" {
						ref = "main"
					}
					if scenario == "wrong head" {
						head = ""
					}
					if scenario == "changed head" && prCalls > 1 {
						head = strings.Repeat("b", 40)
					}
					mergeable := any(true)
					if scenario == "missing mergeability" {
						mergeable = nil
					}
					if err := json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "open", "mergeable": mergeable, "head": map[string]any{"ref": ref, "sha": head, "repo": map[string]any{"full_name": repo}}, "base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "acme/repo"}}}); err != nil {
						t.Errorf("encode provider fixture: %v", err)
					}
				case r.URL.Path == "/repos/acme/repo/branches/main":
					if _, err := fmt.Fprint(w, `{"protected":true}`); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case r.URL.Path == "/repos/acme/repo/rules/branches/main":
					switch scenario {
					case "rules unavailable":
						w.WriteHeader(403)
					case "rules empty parameters":
						if _, err := fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{}}]`); err != nil {
							t.Error(err)
						}
					case "empty rules":
						if _, err := fmt.Fprint(w, `{}`); err != nil {
							t.Error(err)
						}
					case "rules missing required":
						if _, err := fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"missing"}]}}]`); err != nil {
							t.Error(err)
						}
					case "rules success", "optional failure", "rules only":
						if _, err := fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":1}]}},{"type":"pull_request","parameters":{"required_approving_review_count":2}}]`); err != nil {
							t.Error(err)
						}
					default:
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
						}
					}
				case strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
					if scenario == "rules only" {
						w.WriteHeader(404)
						return
					}
					if scenario == "missing contexts" {
						if _, err := fmt.Fprint(w, `{"checks":[]}`); err != nil {
							t.Error(err)
						}
						return
					}
					if scenario == "missing checks" {
						if _, err := fmt.Fprint(w, `{"contexts":[]}`); err != nil {
							t.Error(err)
						}
						return
					}
					if scenario == "empty policy" {
						if _, err := fmt.Fprint(w, `{}`); err != nil {
							t.Error(err)
						}
						return
					}
					if scenario == "no CI" || scenario == "allow no CI" {
						if _, err := fmt.Fprint(w, `{"contexts":[],"checks":[]}`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
					} else {
						if _, err := fmt.Fprint(w, `{"contexts":["build"],"checks":[{"context":"build","app_id":1}]}`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
					}
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					if scenario == "missing required" || scenario == "no CI" || scenario == "allow no CI" {
						if _, err := fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
						return
					}
					if scenario == "optional failure" {
						if _, err := fmt.Fprintf(w, `{"total_count":2,"check_runs":[{"name":"build","head_sha":%q,"status":"completed","conclusion":"success","app":{"id":1}},{"name":"optional","head_sha":%q,"status":"completed","conclusion":"failure"}]}`, sha, sha); err != nil {
							t.Error(err)
						}
						return
					}
					state, conclusion := "completed", "success"
					if scenario == "pending" {
						state = "in_progress"
					}
					if scenario == "failed" {
						conclusion = "failure"
					}
					if _, err := fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"name":"build","head_sha":%q,"status":%q,"conclusion":%q,"app":{"id":1}}]}`, sha, state, conclusion); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case strings.HasSuffix(r.URL.Path, "/status"):
					if _, err := fmt.Fprintf(w, `{"sha":%q,"state":"pending","total_count":0,"statuses":[]}`, sha); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					switch scenario {
					case "pending review":
						if _, err := fmt.Fprint(w, `[{"state":"PENDING","user":{"id":1}}]`); err != nil {
							t.Error(err)
						}
					case "changes requested":
						if _, err := fmt.Fprint(w, `[{"state":"CHANGES_REQUESTED","user":{"id":1}}]`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
					case "dismissed":
						if _, err := fmt.Fprint(w, `[{"state":"CHANGES_REQUESTED","user":{"id":1}},{"state":"DISMISSED","user":{"id":1}}]`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
					default:
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
					}
				case r.URL.Path == "/graphql":
					if r.Method != "POST" {
						t.Error("graphql must POST")
					}
					if scenario == "graphql error" {
						if _, err := fmt.Fprint(w, `{"errors":[{"message":"denied"}]}`); err != nil {
							t.Errorf("write provider fixture: %v", err)
						}
						return
					}
					if _, err := fmt.Fprintf(w, `{"data":{"repository":{"pullRequest":{"headRefOid":%q,"reviewThreads":{"nodes":[{"isResolved":%t}],"pageInfo":{"hasNextPage":false}}}}}}`, sha, scenario != "red thread"); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			transport := server.Client().Transport
			c := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					t.Errorf("credential destination %s", r.URL.Host)
				}
				copied := r.Clone(r.Context())
				u := *r.URL
				copied.URL = &u
				copied.URL.Scheme = "https"
				copied.URL.Host = strings.TrimPrefix(server.URL, "https://")
				return transport.RoundTrip(copied)
			})}
			ctx := context.Background()
			if scenario == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			ok, err := ProviderVerifier(s, c)(ctx, a)
			want := scenario == "success" || scenario == "dismissed" || scenario == "allow no CI" || scenario == "rules success" || scenario == "optional failure" || scenario == "pending review" || scenario == "rules only"
			if ok != want || (want && err != nil) || (!want && err == nil) {
				t.Fatalf("ok=%v err=%v want=%v", ok, err, want)
			}
			if (scenario == "bad URL" || scenario == "missing token" || scenario == "legacy task" || scenario == "cancelled") && calls != 0 {
				t.Fatal("made request before validating input")
			}
			if err != nil && strings.Contains(err.Error(), "test-secret") {
				t.Fatal("credential leaked")
			}
		})
	}
}

func TestProviderRepository(t *testing.T) {
	for _, raw := range []string{"https://github.com/acme/repo.git", "git@github.com:acme/repo.git", "ssh://git@github.com/acme/repo"} {
		host, owner, name, err := providerRepository(raw)
		if err != nil || host != "github.com" || owner != "acme" || name != "repo" {
			t.Fatalf("%s: %s %s %s %v", raw, host, owner, name, err)
		}
	}
	for _, raw := range []string{"https://user:secret@github.com/acme/repo", "https://github.com/acme/../repo", "https://github.com/acme/%2frepo", "http://github.com/acme/repo", "https://github.com/acme/repo?token=secret"} {
		if _, _, _, err := providerRepository(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestProviderReaderSafety(t *testing.T) {
	for _, scenario := range []string{"pagination", "timeout", "oversize", "status"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "pagination":
					w.Header().Set("Link", `<https://evil.example/>; rel="next"`)
					if _, err := fmt.Fprint(w, `[]`); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case "timeout":
					<-r.Context().Done()
				case "oversize":
					if _, err := fmt.Fprint(w, strings.Repeat(" ", 4<<20+1)); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case "status":
					w.WriteHeader(403)
					if _, err := fmt.Fprint(w, "secret"); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			p := providerReader{ctx: ctx, client: server.Client(), api: server.URL, token: "secret"}
			var out any
			if err := p.read("/", &out); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe response %v", err)
			}
		})
	}
}

func TestForgejoVerifier(t *testing.T) {
	for _, scenario := range []string{"success", "no reviews", "resolved", "unresolved", "unknown resolution", "dismissed", "changes requested", "missing required", "failed", "pending", "no CI", "empty policy", "changed head", "missing comments", "optional failure", "requested review"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("JAWA_FORGEJO_TOKEN", "secret")
			s, err := OpenStore(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			p, err := s.CreateProjectDetails("test", "forgejo", "https://forge.example/acme/repo", "main")
			if err != nil {
				t.Fatal(err)
			}
			card, err := s.CreateCard(p.ID, "test", "")
			if err != nil {
				t.Fatal(err)
			}
			branch := fmt.Sprintf("jawa/card/%s/attempt/1", card.ID)
			sha := strings.Repeat("a", 40)
			data, err := json.Marshal(client.Task{Version: 1, TaskID: "task", Text: "Implement\nRepository: " + p.Repo + "\nBase branch: main\nProvider: forgejo\nIssue: \nBranch: " + branch})
			if err != nil {
				t.Fatal(err)
			}
			paths := map[string]bool{}
			prCalls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths[r.URL.Path] = true
				if r.Header.Get("Authorization") != "token secret" {
					t.Error("wrong auth")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					prCalls++
					head := sha
					if scenario == "changed head" && prCalls > 1 {
						head = strings.Repeat("b", 40)
					}
					if _, err := fmt.Fprintf(w, `{"number":1,"state":"open","mergeable":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","repo":{"full_name":"acme/repo"}}}`, branch, head); err != nil {
						t.Errorf("write provider fixture: %v", err)
					}
				case strings.HasSuffix(r.URL.Path, "/branches/main"):
					if scenario == "empty policy" {
						if _, err := fmt.Fprint(w, `{}`); err != nil {
							t.Error(err)
						}
						return
					}
					if _, err := fmt.Fprint(w, `{"name":"main","protected":true,"enable_status_check":true,"status_check_contexts":["build"],"required_approvals":2}`); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/status"):
					context, state := "build", "success"
					switch scenario {
					case "missing required":
						context = "other"
					case "failed":
						state = "failure"
					case "pending":
						state = "pending"
					case "no CI":
						if _, err := fmt.Fprintf(w, `{"sha":%q,"state":"pending","total_count":0,"statuses":[]}`, sha); err != nil {
							t.Error(err)
						}
						return
					case "optional failure":
						if _, err := fmt.Fprintf(w, `{"sha":%q,"state":"failure","total_count":2,"statuses":[{"context":"build","status":"success"},{"context":"optional","status":"failure"}]}`, sha); err != nil {
							t.Error(err)
						}
						return
					}
					if _, err := fmt.Fprintf(w, `{"sha":%q,"state":%q,"total_count":1,"statuses":[{"context":%q,"status":%q}]}`, sha, state, context, state); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/reviews"):
					if scenario == "no reviews" {
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
						}
						return
					}
					state, count := "COMMENT", 0
					switch scenario {
					case "requested review":
						state = "REQUEST_REVIEW"
					case "changes requested", "dismissed":
						state = "REQUEST_CHANGES"
					case "resolved", "unresolved", "unknown resolution", "missing comments":
						count = 1
					}
					if _, err := fmt.Fprintf(w, `[{"id":1,"state":%q,"dismissed":%t,"comments_count":%d,"user":{"id":1}}]`, state, scenario == "dismissed", count); err != nil {
						t.Error(err)
					}
				case strings.HasSuffix(r.URL.Path, "/reviews/1/comments"):
					switch scenario {
					case "resolved":
						if _, err := fmt.Fprint(w, `[{"id":1,"pull_request_review_id":1,"resolver":{"id":2}}]`); err != nil {
							t.Error(err)
						}
					case "unresolved":
						if _, err := fmt.Fprint(w, `[{"id":1,"pull_request_review_id":1,"resolver":null}]`); err != nil {
							t.Error(err)
						}
					case "unknown resolution":
						if _, err := fmt.Fprint(w, `[{"id":1,"pull_request_review_id":1}]`); err != nil {
							t.Error(err)
						}
					default:
						if _, err := fmt.Fprint(w, `[]`); err != nil {
							t.Error(err)
						}
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			transport := server.Client().Transport
			c := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "forge.example" {
					t.Error("wrong host")
				}
				copied := r.Clone(r.Context())
				u := *r.URL
				copied.URL = &u
				u.Host = strings.TrimPrefix(server.URL, "https://")
				return transport.RoundTrip(copied)
			})}
			ok, err := ProviderVerifier(s, c)(context.Background(), Attempt{CardID: card.ID, Number: 1, TaskID: "task", TaskJSON: string(data), PRURL: "https://forge.example/acme/repo/pulls/1"})
			want := scenario == "success" || scenario == "no reviews" || scenario == "resolved" || scenario == "dismissed" || scenario == "optional failure" || scenario == "requested review"
			if ok != want || (want && err != nil) || (!want && err == nil) {
				t.Fatalf("ok=%v err=%v want=%v", ok, err, want)
			}
			if want && !paths["/api/v1/repos/acme/repo/pulls/1/reviews"] {
				t.Fatal("missing review reads")
			}
		})
	}
}
