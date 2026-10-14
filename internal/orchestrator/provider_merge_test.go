package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderPRMerger(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, method := range []string{"merge", "squash", "rebase"} {
			for _, scenario := range []string{"success", "already merged", "deleted head repo", "changed head", "wrong repo", "wrong base", "wrong branch", "wrong number", "draft", "closed", "conflict", "unknown", "invalid method", "missing credentials", "redirect", "401", "403", "404", "405", "409", "422", "500", "transport", "malformed", "oversize", "refused", "confirmation fails", "confirmation head changes", "cancelled"} {
				t.Run(provider+"/"+method+"/"+scenario, func(t *testing.T) {
					s, a := conflictFixture(t, provider)
					branch, _ := assignedBranch(a)
					head := strings.Repeat("a", 40)
					writes, reads := 0, 0
					transport := providerTransport(func(r *http.Request) (*http.Response, error) {
						if r.Context().Err() != nil {
							return nil, r.Context().Err()
						}
						expectedPath := "/repos/acme/repo/pulls/7"
						if provider == "forgejo" {
							expectedPath = "/api/v1" + expectedPath
						}
						response := func(status int, body string) (*http.Response, error) {
							return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
						}
						if r.Method != "GET" {
							writes++
							if r.URL.Path != expectedPath+"/merge" {
								t.Error("wrong endpoint", r.URL)
							}
							auth, verb := "Bearer test-token", http.MethodPut
							if provider == "forgejo" {
								auth, verb = "token test-token", http.MethodPost
							}
							if r.Header.Get("Authorization") != auth || r.Method != verb {
								t.Error("wrong auth or verb")
							}
							var payload map[string]any
							if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
								t.Fatal(err)
							}
							if provider == "forgejo" {
								if payload["Do"] != method || payload["head_commit_id"] != head {
									t.Error("missing head/method", payload)
								}
								for _, flag := range []string{"force_merge", "merge_when_checks_succeed", "delete_branch_after_merge"} {
									if payload[flag] != false {
										t.Error("unsafe flag", payload)
									}
								}
							} else if payload["sha"] != head || payload["merge_method"] != method {
								t.Error("missing head/method")
							}
							switch scenario {
							case "redirect":
								return response(302, "")
							case "401", "403", "404", "405", "409", "422", "500":
								var status int
								_, _ = fmt.Sscan(scenario, &status)
								return response(status, "test-token private error")
							case "transport":
								return nil, fmt.Errorf("test-token private transport")
							case "malformed":
								return response(200, "invalid test-token")
							case "oversize":
								return response(200, strings.Repeat("x", (4<<20)+1))
							case "refused":
								if provider == "github" {
									return response(200, `{"merged":false,"message":"test-token"}`)
								}
								return response(409, "")
							}
							if provider == "forgejo" {
								return response(200, "")
							}
							return response(200, `{"merged":true}`)
						}
						reads++
						if r.URL.Path != expectedPath {
							t.Error("wrong read endpoint")
						}
						pr := map[string]any{"number": 7, "state": "open", "merged": false, "mergeable": true, "head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": "acme/repo"}}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("b", 40), "repo": map[string]any{"full_name": "acme/repo"}}}
						if writes > 0 || scenario == "already merged" {
							pr["state"], pr["merged"] = "closed", true
						}
						switch scenario {
						case "changed head":
							pr["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
						case "wrong repo":
							pr["head"].(map[string]any)["repo"] = map[string]any{"full_name": "fork/repo"}
						case "wrong base":
							pr["base"].(map[string]any)["ref"] = "other"
						case "wrong branch":
							pr["head"].(map[string]any)["ref"] = "other"
						case "wrong number":
							pr["number"] = 8
						case "draft":
							pr["draft"] = true
						case "closed":
							pr["state"] = "closed"
						case "conflict":
							pr["mergeable"], pr["mergeable_state"] = false, "dirty"
						case "unknown":
							pr["mergeable"] = nil
						case "deleted head repo":
							if writes > 0 {
								pr["head"].(map[string]any)["repo"] = nil
							}
						case "confirmation fails":
							if writes > 0 {
								return response(500, "test-token")
							}
						case "confirmation head changes":
							if writes > 0 {
								pr["head"].(map[string]any)["sha"] = strings.Repeat("c", 40)
							}
						}
						data, _ := json.Marshal(pr)
						return response(200, string(data))
					})
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					selected := method
					if scenario == "invalid method" {
						selected = "force"
					}
					if scenario == "cancelled" {
						cancel()
					}
					if scenario == "missing credentials" {
						t.Setenv("JAWA_GITHUB_TOKEN", "")
						t.Setenv("GITHUB_TOKEN", "")
						t.Setenv("JAWA_FORGEJO_TOKEN", "")
						t.Setenv("FORGEJO_TOKEN", "")
					}
					result := ProviderPRMerger(s, &http.Client{Transport: transport})(ctx, a, head, selected)
					want, maxWrites := "rejected", 0
					switch scenario {
					case "success", "deleted head repo":
						want, maxWrites = "merged", 1
					case "already merged":
						want = "merged"
					case "redirect", "500", "transport", "malformed", "oversize", "confirmation fails", "confirmation head changes":
						want, maxWrites = "uncertain", 1
					case "401", "403", "404", "405", "409", "422", "refused":
						maxWrites = 1
					}
					if result != want || writes != maxWrites {
						t.Fatalf("result %s want %s writes %d want %d reads %d", result, want, writes, maxWrites, reads)
					}
				})
			}
		}
	}
}

func TestProviderMergeTLSAndCancellation(t *testing.T) {
	for _, phase := range []string{"preflight", "write", "confirmation"} {
		t.Run(phase, func(t *testing.T) {
			s, a := conflictFixture(t, "forgejo")
			branch, _ := assignedBranch(a)
			head := strings.Repeat("a", 40)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var writes atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
					if phase == "write" {
						cancel()
						<-ctx.Done()
						return
					}
					w.WriteHeader(http.StatusOK)
					return
				}
				if phase == "preflight" || (phase == "confirmation" && writes.Load() > 0) {
					cancel()
					return
				}
				merged, state := false, "open"
				if writes.Load() > 0 {
					merged, state = true, "closed"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": state, "merged": merged, "mergeable": true, "head": map[string]any{"ref": branch, "sha": head, "repo": map[string]any{"full_name": "acme/repo"}}, "base": map[string]any{"ref": "main", "sha": strings.Repeat("b", 40), "repo": map[string]any{"full_name": "acme/repo"}}})
			}))
			defer server.Close()
			host := strings.TrimPrefix(server.URL, "https://")
			oldRepo := "https://forge.example/acme/repo.git"
			newRepo := server.URL + "/acme/repo.git"
			_, err := s.db.Exec(`UPDATE projects SET repo=?`, newRepo)
			if err != nil {
				t.Fatal(err)
			}
			a.TaskJSON = strings.ReplaceAll(a.TaskJSON, oldRepo, newRepo)
			a.PRURL = "https://" + host + "/acme/repo/pulls/7"
			result := ProviderPRMerger(s, server.Client())(ctx, a, head, "merge")
			want, maxWrites := "uncertain", 1
			if phase == "preflight" {
				want, maxWrites = "rejected", 0
			}
			if result != want || int(writes.Load()) != maxWrites {
				t.Fatalf("%s writes %d", result, writes.Load())
			}
		})
	}
}
