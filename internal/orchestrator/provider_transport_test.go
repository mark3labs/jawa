package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderHTTPClientPolicy(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	supplied := &http.Client{Jar: jar}
	c := providerHTTPClient(supplied)
	if c.Timeout != 20*time.Second || c.Jar != nil || c.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("provider client policy not enforced")
	}
	if supplied.Timeout != 0 || supplied.Jar != jar || supplied.CheckRedirect != nil {
		t.Fatal("supplied client mutated")
	}
}

// Exercise each entry point with a real HTTP exchange, using a trusted test
// transport to route configured provider origins to the local fixture only.
func TestProviderTransportPolicy(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, caller := range []string{"identity", "merge", "conflict", "list", "create"} {
			for _, scenario := range []string{"redirect", "oversize", "cookies"} {
				t.Run(provider+"/"+caller+"/"+scenario, func(t *testing.T) {
					s, a := conflictFixture(t, provider)
					branch, err := assignedBranch(a)
					if err != nil {
						t.Fatal(err)
					}
					sha := strings.Repeat("a", 40)
					var redirected, calls, writes atomic.Int32
					other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						redirected.Add(1)
						w.WriteHeader(http.StatusInternalServerError)
					}))
					defer other.Close()
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Header.Get("Cookie") != "" {
							t.Error("provider sent cookies")
						}
						auth := "Bearer test-token"
						if provider == "forgejo" {
							auth = "token test-token"
						}
						if r.Header.Get("Authorization") != auth {
							t.Error("missing provider authentication")
						}
						http.SetCookie(w, &http.Cookie{Name: "session", Value: "must-not-return", Path: "/"})
						isPR := strings.HasSuffix(r.URL.Path, "/pulls/7")
						// Merge must pass preflight so its write transport is tested too.
						if caller != "merge" || !isPR {
							if scenario == "redirect" {
								w.Header().Set("Location", other.URL)
								w.WriteHeader(http.StatusFound)
								return
							}
							if scenario == "oversize" {
								if caller == "create" {
									w.WriteHeader(http.StatusCreated)
								}
								body := `[]`
								if caller == "create" {
									body = `{"number":7}`
								} else if caller == "merge" {
									body = `{"merged":false}`
								} else if isPR {
									body = fmt.Sprintf(`{"number":7,"state":"open","mergeable":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","sha":%q,"repo":{"full_name":"acme/repo"}}}`, branch, sha, sha)
								}
								_, _ = fmt.Fprint(w, body+strings.Repeat(" ", (4<<20)+1))
								return
							}
						}
						switch {
						case isPR:
							merged := writes.Load() > 0
							state := "open"
							if merged {
								state = "closed"
							}
							err := json.NewEncoder(w).Encode(map[string]any{
								"number": 7, "state": state, "merged": merged, "mergeable": true,
								"head": map[string]any{"ref": branch, "sha": sha, "repo": map[string]any{"full_name": "acme/repo"}},
								"base": map[string]any{"ref": "main", "sha": sha, "repo": map[string]any{"full_name": "acme/repo"}},
							})
							if err != nil {
								t.Error(err)
							}
						case strings.HasSuffix(r.URL.Path, "/merge"):
							writes.Add(1)
							_, _ = fmt.Fprint(w, `{"merged":true}`)
						case r.Method == http.MethodPost:
							w.WriteHeader(http.StatusCreated)
							_, _ = fmt.Fprint(w, `{"number":7}`)
						default:
							_, _ = fmt.Fprint(w, `[]`)
						}
					}))
					defer server.Close()
					local, err := url.Parse(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					jar, err := cookiejar.New(nil)
					if err != nil {
						t.Fatal(err)
					}
					host := "api.github.com"
					repo := "https://github.com/acme/repo.git"
					if provider == "forgejo" {
						host, repo = "forge.example", "https://forge.example/acme/repo.git"
					}
					origin, err := url.Parse("https://" + host)
					if err != nil {
						t.Fatal(err)
					}
					jar.SetCookies(origin, []*http.Cookie{{Name: "existing", Value: "must-not-send"}})
					c := &http.Client{Jar: jar, Timeout: 0, Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
						if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 20*time.Second {
							t.Error("provider request lacks bounded deadline")
						}
						clone := r.Clone(r.Context())
						if clone.URL.Host == host {
							clone.URL.Scheme, clone.URL.Host = local.Scheme, local.Host
						}
						return http.DefaultTransport.RoundTrip(clone)
					})}
					project := Project{Provider: provider, Repo: repo}
					identity := ProviderIdentity(s, c)
					merger := ProviderPRMerger(s, c)
					conflict := ProviderConflictChecker(s, c)
					for range 2 { // Also prove Set-Cookie from the first invocation is ignored.
						var failed bool
						switch caller {
						case "identity":
							_, err := identity(t.Context(), a, a.PRURL)
							failed = err != nil
						case "merge":
							state := merger(t.Context(), a, sha, "squash")
							failed = state != "merged"
							if scenario != "cookies" && state != "uncertain" {
								t.Errorf("write outcome = %s", state)
							}
						case "conflict":
							_, err := conflict(t.Context(), a)
							failed = err != nil
						case "list":
							_, err := listProjectIssues(t.Context(), project, c)
							failed = err != nil
						case "create":
							_, err := createProjectIssue(t.Context(), project, "title", "body", c, nil)
							failed = err != nil
							if scenario != "cookies" {
								e, ok := err.(*issueCreateError)
								if !ok || !e.Uncertain {
									t.Errorf("write outcome = %v", err)
								}
							}
						}
						if failed != (scenario != "cookies") {
							t.Errorf("failed=%v for %s", failed, scenario)
						}
					}
					if calls.Load() < 2 || redirected.Load() != 0 {
						t.Errorf("calls=%d redirects=%d", calls.Load(), redirected.Load())
					}
					if c.Timeout != 0 || c.Jar != jar || c.CheckRedirect != nil {
						t.Fatal("supplied client mutated")
					}
				})
			}
		}
	}
}

func TestProviderTransportErrors(t *testing.T) {
	for _, scenario := range []string{"transport", "cancelled", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := providerReader{ctx: ctx, client: providerHTTPClient(&http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) {
				if scenario == "cancelled" {
					cancel()
				}
				if scenario == "unreadable" {
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(providerErrorReader{})}, nil
				}
				return nil, errors.New("private transport diagnostic")
			})})}
			_, _, data, err := reader.do(http.MethodGet, "https://api.github.com/test", nil)
			if err == nil || data != nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe error: %v", err)
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
		})
	}
}

type providerErrorReader struct{}

func (providerErrorReader) Read([]byte) (int, error) { return 0, errors.New("private body diagnostic") }
