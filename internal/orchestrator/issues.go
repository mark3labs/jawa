package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type projectIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

// listProjectIssues uses only configured repository coordinates, never API-supplied
// issue or pagination URLs. The supplied client provides transport, not policy.
func listProjectIssues(ctx context.Context, project Project, supplied *http.Client) ([]projectIssue, error) {
	host, owner, name, err := providerRepository(project.Repo)
	if err != nil {
		return nil, err
	}
	var api, token, auth, pageSize string
	switch project.Provider {
	case "github":
		if host != "github.com" {
			return nil, errors.New("issues: only github.com is supported")
		}
		api, auth, pageSize = "https://api.github.com", "Bearer ", "per_page=100"
		token = os.Getenv("JAWA_GITHUB_TOKEN")
		if token == "" {
			token = os.Getenv("GITHUB_TOKEN")
		}
	case "forgejo":
		api, auth, pageSize = "https://"+host+"/api/v1", "token ", "limit=100&type=issues"
		token = os.Getenv("JAWA_FORGEJO_TOKEN")
	default:
		return nil, errors.New("issues: unsupported provider")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("issues: credentials missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout = 20 * time.Second
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	issues := []projectIssue{}
	seen := map[int]bool{}
	for page := 1; page <= 20; page++ {
		target := api + "/repos/" + owner + "/" + name + "/issues?state=open&" + pageSize + "&page=" + strconv.Itoa(page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return nil, errors.New("issues: invalid request")
		}
		req.Header.Set("Authorization", auth+token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("issues: HTTP request failed")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		_ = resp.Body.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if resp.StatusCode != http.StatusOK {
			return nil, errors.New("issues: API request unsuccessful")
		}
		if readErr != nil || len(data) > 4<<20 {
			return nil, errors.New("issues: response exceeds limit or unreadable")
		}
		var entries []struct {
			Number       int
			Title, State string
			PullRequest  json.RawMessage `json:"pull_request"`
			IsPull       bool            `json:"is_pull"`
		}
		if json.Unmarshal(data, &entries) != nil || entries == nil || len(entries) > 100 {
			return nil, errors.New("issues: malformed API response")
		}
		for _, entry := range entries {
			if entry.State != "open" || entry.IsPull || (len(entry.PullRequest) != 0 && string(entry.PullRequest) != "null") {
				continue
			}
			if entry.Number < 1 || strings.TrimSpace(entry.Title) == "" || len(entry.Title) > 4096 || strings.ContainsFunc(entry.Title, unicode.IsControl) {
				return nil, errors.New("issues: invalid issue number or title")
			}
			if !seen[entry.Number] {
				issues = append(issues, projectIssue{Number: entry.Number, Title: entry.Title, URL: "https://" + host + "/" + owner + "/" + name + "/issues/" + strconv.Itoa(entry.Number)})
				seen[entry.Number] = true
			}
		}
		// A full page or a next-page hint requires another fixed API page. Link
		// targets are deliberately ignored, including cross-host targets.
		if len(entries) < 100 && !strings.Contains(resp.Header.Get("Link"), `rel="next"`) {
			return issues, nil
		}
	}
	return nil, errors.New("issues: pagination exceeds supported limit")
}

func (a *app) projectIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	projects, err := a.s.Projects()
	if err != nil {
		http.Error(w, "Unable to load projects.", http.StatusInternalServerError)
		return
	}
	for _, project := range projects {
		if project.ID != r.URL.Query().Get("project_id") {
			continue
		}
		issues, err := listProjectIssues(r.Context(), project, a.issueClient)
		if err != nil {
			http.Error(w, "Unable to load project issues.", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Issues []projectIssue `json:"issues"`
		}{issues})
		return
	}
	http.NotFound(w, r)
}
