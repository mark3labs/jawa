package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type projectIssue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

// listProjectIssues uses only configured repository coordinates, never API-supplied
// issue or pagination URLs. The supplied client provides transport, not policy.
func listProjectIssues(ctx context.Context, project Project, supplied *http.Client, settings ...map[string]string) ([]projectIssue, error) {
	overrides := map[string]string{}
	if len(settings) > 0 {
		overrides = settings[0]
	}
	endpoint, err := resolveProviderEndpoint(project.Provider, project.Repo, overrides)
	if err != nil {
		return nil, fmt.Errorf("issues: %w", err)
	}
	if err := endpoint.requireCredentials(); err != nil {
		return nil, fmt.Errorf("issues: %w", err)
	}
	pageSize := "per_page=100"
	if endpoint.Forgejo {
		pageSize = "limit=100&type=issues"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p := providerReader{ctx: ctx, client: providerHTTPClient(supplied), authPrefix: endpoint.AuthPrefix, token: endpoint.Token}
	issues := []projectIssue{}
	seen := map[int]bool{}
	for page := 1; page <= 20; page++ {
		target := endpoint.repoAPI() + "/issues?state=open&" + pageSize + "&page=" + strconv.Itoa(page)
		status, header, data, err := p.do(http.MethodGet, target, nil)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if status == 0 {
			return nil, errors.New("issues: HTTP request failed")
		}
		if status != http.StatusOK {
			return nil, errors.New("issues: API request unsuccessful")
		}
		if err != nil {
			return nil, errors.New("issues: response exceeds limit or unreadable")
		}
		var entries []struct {
			Number             int
			Title, State, Body string
			PullRequest        json.RawMessage `json:"pull_request"`
			IsPull             bool            `json:"is_pull"`
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
				issues = append(issues, projectIssue{Number: entry.Number, Title: entry.Title, Body: entry.Body, URL: endpoint.WebBase + "/issues/" + strconv.Itoa(entry.Number)})
				seen[entry.Number] = true
			}
		}
		// A full page or a next-page hint requires another fixed API page. Link
		// targets are deliberately ignored, including cross-host targets.
		if len(entries) < 100 && !strings.Contains(header.Get("Link"), `rel="next"`) {
			return issues, nil
		}
	}
	return nil, errors.New("issues: pagination exceeds supported limit")
}
