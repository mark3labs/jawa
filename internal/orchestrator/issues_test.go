package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func issueResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestListProjectIssuesProviders(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			t.Setenv("JAWA_GITHUB_TOKEN", "")
			t.Setenv("GITHUB_TOKEN", "fallback-test-token")
			t.Setenv("JAWA_FORGEJO_TOKEN", "forgejo-test-token")
			repo, host, root, auth, size := "git@github.com:acme/repo.git", "api.github.com", "/repos/acme/repo/issues", "Bearer fallback-test-token", "per_page"
			if provider == "forgejo" {
				repo, host, root, auth, size = "https://forge.example/acme/repo.git", "forge.example", "/api/v1/repos/acme/repo/issues", "token forgejo-test-token", "limit"
			}
			calls := 0
			jar, _ := cookiejar.New(nil)
			u, _ := url.Parse("https://" + host)
			jar.SetCookies(u, []*http.Cookie{{Name: "secret-cookie", Value: "must-not-send"}})
			c := &http.Client{Jar: jar, Timeout: time.Hour, Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Scheme != "https" || r.URL.Host != host || r.URL.Path != root || r.URL.Query().Get("page") != fmt.Sprint(calls) || r.URL.Query().Get(size) != "100" || r.URL.Query().Get("state") != "open" {
					t.Fatalf("unexpected API request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != auth || r.Header.Get("Cookie") != "" {
					t.Fatal("wrong credentials or cookies leaked")
				}
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 20*time.Second {
					t.Fatal("missing bounded deadline")
				}
				if calls == 1 {
					resp := issueResponse(`[{"number":7,"title":"Open issue","state":"open","html_url":"javascript:evil"},{"number":8,"title":"PR","state":"open","pull_request":{}},{"number":9,"title":"Closed","state":"closed"},{"number":10,"title":"PR","state":"open","is_pull":true}]`)
					resp.Header.Set("Link", `<https://evil.example/steal>; rel="next"`)
					return resp, nil
				}
				return issueResponse(`[{"number":11,"title":"Second issue","state":"open","pull_request":null},{"number":7,"title":"duplicate","state":"open"}]`), nil
			})}
			issues, err := listProjectIssues(t.Context(), Project{Provider: provider, Repo: repo}, c)
			if err != nil || len(issues) != 2 || calls != 2 {
				t.Fatalf("issues=%+v calls=%d err=%v", issues, calls, err)
			}
			webHost := "github.com"
			if provider == "forgejo" {
				webHost = host
			}
			if issues[0].Number != 7 || issues[0].Title != "Open issue" || issues[0].URL != "https://"+webHost+"/acme/repo/issues/7" {
				t.Fatalf("untrusted issue: %+v", issues[0])
			}
			if c.Jar != jar || c.Timeout != time.Hour || c.CheckRedirect != nil {
				t.Fatal("supplied client mutated")
			}
		})
	}
}

func TestListProjectIssuesErrors(t *testing.T) {
	for _, scenario := range []string{"token", "unsupported", "unsafe repo", "wrong github host", "redirect", "http", "transport", "malformed", "null", "number", "title", "control", "oversized", "cancelled", "page limit", "full page"} {
		t.Run(scenario, func(t *testing.T) {
			t.Setenv("JAWA_GITHUB_TOKEN", "private-test-token")
			t.Setenv("GITHUB_TOKEN", "")
			p := Project{Provider: "github", Repo: "https://github.com/acme/repo"}
			switch scenario {
			case "token":
				t.Setenv("JAWA_GITHUB_TOKEN", "")
			case "unsupported":
				p.Provider = "other"
			case "unsafe repo":
				p.Repo = "https://user:password@github.com/acme/repo?token=secret"
			case "wrong github host":
				p.Repo = "https://evil.example/acme/repo"
			}
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := issueResponse(`[]`)
				switch scenario {
				case "redirect":
					resp.StatusCode = 302
					resp.Header.Set("Location", "https://evil.example/steal")
				case "http":
					resp.StatusCode = 401
				case "transport":
					return nil, errors.New("private-test-token from transport")
				case "malformed":
					resp = issueResponse(`{`)
				case "null":
					resp = issueResponse(`null`)
				case "number":
					resp = issueResponse(`[{"number":0,"title":"x","state":"open"}]`)
				case "title":
					resp = issueResponse(`[{"number":1,"title":"  ","state":"open"}]`)
				case "control":
					resp = issueResponse(`[{"number":1,"title":"x\u0000","state":"open"}]`)
				case "oversized":
					resp = issueResponse(strings.Repeat(" ", (4<<20)+1))
				case "cancelled":
					cancel()
					<-r.Context().Done()
					return nil, r.Context().Err()
				case "page limit":
					resp.Header.Set("Link", `<https://evil.example>; rel="next"`)
				case "full page":
					if calls == 1 {
						entries := make([]map[string]any, 100)
						for i := range entries {
							entries[i] = map[string]any{"number": i + 1, "title": "issue", "state": "open"}
						}
						data, _ := json.Marshal(entries)
						resp = issueResponse(string(data))
					}
				}
				return resp, nil
			})}
			issues, err := listProjectIssues(ctx, p, c)
			if scenario == "full page" {
				if err != nil || len(issues) != 100 || calls != 2 {
					t.Fatalf("full page: %d %d %v", len(issues), calls, err)
				}
				return
			}
			if err == nil || issues != nil || strings.Contains(err.Error(), "private-test-token") {
				t.Fatalf("unsafe result: %+v %v", issues, err)
			}
			if scenario == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			if scenario == "page limit" && calls != 20 {
				t.Fatalf("page limit: %d", calls)
			}
			if scenario == "redirect" && calls != 1 {
				t.Fatalf("redirect followed: %d", calls)
			}
			if (scenario == "token" || scenario == "unsupported" || scenario == "unsafe repo" || scenario == "wrong github host") && calls != 0 {
				t.Fatal("unsafe network call")
			}
		})
	}
}

func TestProjectIssuesEndpoint(t *testing.T) {
	t.Setenv("JAWA_GITHUB_TOKEN", "test-token")
	a := testApp(t)
	session, _ := setupTest(t, a)
	p, err := a.s.CreateProjectDetails("demo", "github", "https://github.com/acme/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	body := `[]`
	a.issueClient = &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) { calls++; return issueResponse(body), nil })}
	path := "/projects/issues?project_id=" + p.ID
	status(t, request(a, "GET", path, nil), 401)
	status(t, request(a, "GET", "/projects/issues?project_id=missing", nil, session), 404)
	status(t, request(a, "GET", "/projects/issues", nil, session), 404)
	w := request(a, "POST", path, nil, session)
	status(t, w, 405)
	if w.Header().Get("Allow") != "GET" || calls != 0 {
		t.Fatal("invalid requests reached provider")
	}
	w = request(a, "GET", path, nil, session)
	status(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"issues":[]}` || w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %v %s", w.Header(), w.Body.String())
	}
	body = `[{"number":3,"title":"Issue","state":"open","html_url":"https://evil.example"}]`
	w = request(a, "GET", path, nil, session)
	status(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"issues":[{"number":3,"title":"Issue","url":"https://github.com/acme/repo/issues/3"}]}` {
		t.Fatalf("JSON contract: %s", w.Body.String())
	}
	body = `private-test-token malformed`
	w = request(a, "GET", path, nil, session)
	status(t, w, 502)
	if strings.TrimSpace(w.Body.String()) != "Unable to load project issues." {
		t.Fatalf("unsafe error: %s", w.Body.String())
	}
}

func TestListProjectIssuesCommandCredentials(t *testing.T) {
	t.Setenv("JAWA_GITHUB_TOKEN", "env-token")
	c := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer cli-token" {
			t.Fatal("command credential ignored")
		}
		return issueResponse(`[]`), nil
	})}
	_, err := listProjectIssues(t.Context(), Project{Provider: "github", Repo: "https://github.com/acme/repo"}, c, map[string]string{"github-token": "cli-token"})
	if err != nil {
		t.Fatal(err)
	}
}
