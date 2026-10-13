package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCreateIssueProviders(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, body := range []string{"", "  # 日本語\n**Markdown**  "} {
			t.Run(provider+body, func(t *testing.T) {
				p := Project{Provider: provider, Repo: "https://github.com/acme/repo"}
				host, path, auth := "api.github.com", "/repos/acme/repo/issues", "Bearer override"
				if provider == "forgejo" {
					p.Repo = "https://forge.example:8443/acme/repo"
					host, path, auth = "forge.example:8443", "/api/v1/repos/acme/repo/issues", "token override"
				}
				jar, _ := cookiejar.New(nil)
				u, _ := url.Parse("https://" + host)
				jar.SetCookies(u, []*http.Cookie{{Name: "secret", Value: "cookie"}})
				calls := 0
				c := &http.Client{Jar: jar, Timeout: time.Hour, Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "POST" || r.URL.Host != host || r.URL.Path != path || r.Header.Get("Authorization") != auth || r.Header.Get("Cookie") != "" || r.Header.Get("Content-Type") != "application/json" {
						t.Fatal("unsafe request")
					}
					var payload map[string]string
					if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 2 || payload["title"] != "Unicode 日本語" || payload["body"] != body {
						t.Fatalf("payload %+v", payload)
					}
					resp := issueResponse(`{"number":7,"html_url":"https://evil.example"}`)
					resp.StatusCode = 201
					return resp, nil
				})}
				issue, err := createProjectIssue(t.Context(), p, " Unicode 日本語 ", body, c, map[string]string{provider + "-token": "override"})
				if err != nil || calls != 1 || issue.URL != p.Repo+"/issues/7" || c.Jar != jar || c.Timeout != time.Hour {
					t.Fatalf("%+v %v calls %d", issue, err, calls)
				}
			})
		}
	}
}
func TestCreateIssueOutcomes(t *testing.T) {
	for _, scenario := range []string{"403", "422", "500", "redirect", "malformed", "pr", "number", "oversized", "transport", "cancel", "preflight"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			settings := map[string]string{"github-token": "private-token"}
			if scenario == "preflight" {
				settings["github-token"] = ""
				t.Setenv("JAWA_GITHUB_TOKEN", "")
				t.Setenv("GITHUB_TOKEN", "")
			}
			c := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := issueResponse(`{"number":1}`)
				resp.StatusCode = 201
				switch scenario {
				case "403":
					resp.StatusCode = 403
				case "422":
					resp.StatusCode = 422
				case "500":
					resp.StatusCode = 500
				case "redirect":
					resp.StatusCode = 302
					resp.Header.Set("Location", "https://evil.example")
				case "malformed":
					resp.Body = io.NopCloser(strings.NewReader(`{`))
				case "pr":
					resp.Body = io.NopCloser(strings.NewReader(`{"number":1,"pull_request":{}}`))
				case "number":
					resp.Body = io.NopCloser(strings.NewReader(`{"number":0}`))
				case "oversized":
					resp.Body = io.NopCloser(strings.NewReader(strings.Repeat(" ", (4<<20)+1)))
				case "transport":
					return nil, errors.New("private-token transport secret")
				case "cancel":
					cancel()
					return nil, r.Context().Err()
				}
				return resp, nil
			})}
			_, err := createProjectIssue(ctx, Project{Provider: "github", Repo: "https://github.com/acme/repo"}, "title", "body", c, settings)
			var ce *issueCreateError
			if !errors.As(err, &ce) || strings.Contains(err.Error(), "private-token") || ce.Uncertain != (scenario != "403" && scenario != "422" && scenario != "preflight") {
				t.Fatalf("outcome %v", err)
			}
			want := 1
			if scenario == "preflight" {
				want = 0
			}
			if calls != want {
				t.Fatalf("calls %d", calls)
			}
		})
	}
}

func TestCreateIssueCancelledBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	calls := 0
	c := &http.Client{Transport: providerTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected") })}
	_, err := createProjectIssue(ctx, Project{Provider: "github", Repo: "https://github.com/acme/repo"}, "title", "", c, map[string]string{"github-token": "fixture"})
	var ce *issueCreateError
	if !errors.As(err, &ce) || ce.Uncertain || calls != 0 {
		t.Fatalf("%v calls %d", err, calls)
	}
}
