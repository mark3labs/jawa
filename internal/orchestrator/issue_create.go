package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type issueCreateError struct {
	Uncertain bool
	Message   string
}

func (e *issueCreateError) Error() string { return e.Message }

// Preflight resolves only trusted configuration. No provider response controls a target.
func issueCreateTarget(p Project, settings map[string]string) (target, canonical, auth string, err error) {
	host, owner, repo, err := providerRepository(p.Repo)
	if err != nil {
		return "", "", "", &issueCreateError{Message: "Issue creation does not support this repository URL. Turn Create issue off or correct the project."}
	}
	var token, api, prefix string
	switch p.Provider {
	case "github":
		if host != "github.com" {
			return "", "", "", &issueCreateError{Message: "Issue creation supports github.com only."}
		}
		api, prefix, token = "https://api.github.com", "Bearer ", effectiveProviderSetting(settings, "github-token", "JAWA_GITHUB_TOKEN")
	case "forgejo":
		api, prefix, token = "https://"+host+"/api/v1", "token ", effectiveProviderSetting(settings, "forgejo-token", "JAWA_FORGEJO_TOKEN")
	default:
		return "", "", "", &issueCreateError{Message: "Issue creation provider is unsupported."}
	}
	if strings.TrimSpace(token) == "" {
		return "", "", "", &issueCreateError{Message: "Issue creation needs a server provider token with repository Issues read/write permission. Configure Settings or turn Create issue off."}
	}
	return api + "/repos/" + owner + "/" + repo + "/issues", "https://" + host + "/" + owner + "/" + repo + "/issues/", prefix + token, nil
}

// A POST is never retried. Any ambiguous evidence is a potentially completed write.
func createProjectIssue(ctx context.Context, p Project, title, body string, supplied *http.Client, settings map[string]string) (projectIssue, error) {
	target, canonical, auth, err := issueCreateTarget(p, settings)
	if err != nil {
		return projectIssue{}, err
	}
	if strings.TrimSpace(title) == "" {
		return projectIssue{}, &issueCreateError{Message: "Title is required."}
	}
	if ctx.Err() != nil {
		return projectIssue{}, &issueCreateError{Message: "Request cancelled before issue creation."}
	}
	payload, _ := json.Marshal(struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}{strings.TrimSpace(title), body})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return projectIssue{}, &issueCreateError{Message: "Invalid issue request."}
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout = 20 * time.Second
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	uncertain := &issueCreateError{Uncertain: true, Message: "Issue creation may have completed. Refresh issues / check repository, then link the issue or explicitly create an unlinked card. Do not retry creation blindly."}
	resp, err := c.Do(req)
	if err != nil {
		return projectIssue{}, uncertain
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != 408 {
		return projectIssue{}, &issueCreateError{Message: "Provider rejected issue creation. Check repository Issues write permission and draft, then explicitly retry, link an existing issue, or turn creation off."}
	}
	if resp.StatusCode != http.StatusCreated {
		return projectIssue{}, uncertain
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 || ctx.Err() != nil {
		return projectIssue{}, uncertain
	}
	var result struct {
		Number      int
		PullRequest json.RawMessage `json:"pull_request"`
		IsPull      bool            `json:"is_pull"`
	}
	if json.Unmarshal(data, &result) != nil || result.Number < 1 || result.IsPull || (len(result.PullRequest) > 0 && string(result.PullRequest) != "null") {
		return projectIssue{}, uncertain
	}
	return projectIssue{Number: result.Number, Title: strings.TrimSpace(title), Body: body, URL: canonical + strconv.Itoa(result.Number)}, nil
}
