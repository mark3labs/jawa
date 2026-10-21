package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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
	endpoint, err := resolveProviderEndpoint(p.Provider, p.Repo, settings)
	if err != nil {
		message := "Issue creation does not support this repository URL. Use HTTPS with root-path owner/repo (no HTTP or deployment subpaths), or turn Create issue off."
		if errors.Is(err, errUnsupportedHost) {
			message = "Issue creation supports github.com only."
		}
		if errors.Is(err, errUnsupportedProvider) {
			message = "Issue creation provider is unsupported."
		}
		return "", "", "", &issueCreateError{Message: message}
	}
	if endpoint.requireCredentials() != nil {
		return "", "", "", &issueCreateError{Message: "Issue creation needs a server provider token with repository Issues read/write permission. Configure Settings or turn Create issue off."}
	}
	return endpoint.repoAPI() + "/issues", endpoint.WebBase + "/issues/", endpoint.AuthPrefix + endpoint.Token, nil
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
	reader := providerReader{ctx: ctx, client: providerHTTPClient(supplied), token: auth}
	uncertain := &issueCreateError{Uncertain: true, Message: "Issue creation may have completed. Refresh issues / check repository, then link the issue or explicitly create an unlinked card. Do not retry creation blindly."}
	status, _, data, err := reader.do(http.MethodPost, target, payload)
	if status >= 400 && status < 500 && status != 408 {
		return projectIssue{}, &issueCreateError{Message: "Provider rejected issue creation. Check repository Issues write permission and draft, then explicitly retry, link an existing issue, or turn creation off."}
	}
	if status != http.StatusCreated {
		return projectIssue{}, uncertain
	}
	if err != nil || ctx.Err() != nil {
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
