package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	client "github.com/mark3labs/bonnie/client/nats"
)

func effectiveProviderSetting(overrides map[string]string, dashboardKey, envKey string) string {
	if value, ok := overrides[dashboardKey]; ok {
		return value
	}
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	// Jawa-scoped provider tokens fall back to the conventional unprefixed names.
	switch envKey {
	case "JAWA_GITHUB_TOKEN":
		return os.Getenv("GITHUB_TOKEN")
	case "JAWA_FORGEJO_TOKEN":
		return os.Getenv("FORGEJO_TOKEN")
	}
	return ""
}

var (
	errUnsupportedHost     = errors.New("only github.com is supported")
	errUnsupportedProvider = errors.New("unsupported provider")
	errMissingCredentials  = errors.New("credentials missing")
)

type providerEndpoint struct {
	Host, Owner, Name, API, WebBase, AuthPrefix, Token, PullKind string
	Forgejo                                                      bool
}

// resolveProviderEndpoint uses only configured coordinates. Link-only callers do
// not require credentials; network callers must call requireCredentials first.
func resolveProviderEndpoint(provider, repo string, settings map[string]string) (providerEndpoint, error) {
	host, owner, name, err := providerRepository(repo)
	if err != nil {
		return providerEndpoint{}, err
	}
	e := providerEndpoint{Host: host, Owner: owner, Name: name, WebBase: "https://" + host + "/" + owner + "/" + name, PullKind: "pull"}
	switch provider {
	case "github":
		if host != "github.com" {
			return providerEndpoint{}, errUnsupportedHost
		}
		e.API, e.AuthPrefix = "https://api.github.com", "Bearer "
		e.Token = effectiveProviderSetting(settings, "github-token", "JAWA_GITHUB_TOKEN")
	case "forgejo":
		e.API, e.AuthPrefix, e.PullKind, e.Forgejo = "https://"+host+"/api/v1", "token ", "pulls", true
		e.Token = effectiveProviderSetting(settings, "forgejo-token", "JAWA_FORGEJO_TOKEN")
	default:
		return providerEndpoint{}, errUnsupportedProvider
	}
	return e, nil
}

func (e providerEndpoint) repoAPI() string {
	return e.API + "/repos/" + e.Owner + "/" + e.Name
}

func (e providerEndpoint) requireCredentials() error {
	if strings.TrimSpace(e.Token) == "" {
		return errMissingCredentials
	}
	return nil
}

// ProviderVerifier independently reads provider state on every invocation. The
// supplied client supplies transport only; redirects and cookies are disabled.
func providerHTTPClient(supplied *http.Client) *http.Client {
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout = 20 * time.Second
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

// verifiedPR binds readiness evidence to the exact head and validated coordinates.
type verifiedPR struct {
	reader                              providerReader
	path, repository, base, branch, sha string
	number                              int
	merged                              bool
}

func ProviderVerifier(s *Store, supplied *http.Client) Verifier {
	c := providerHTTPClient(supplied)
	return func(ctx context.Context, a Attempt) (bool, error) {
		_, err := verifyProviderPR(ctx, s, c, a)
		return err == nil, err
	}
}

func verifyProviderPR(ctx context.Context, s *Store, c *http.Client, a Attempt) (verifiedPR, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t, err := providerTarget(ctx, s, c, a)
	if err != nil {
		return verifiedPR{}, err
	}
	p := t.reader
	overrides, err := s.providerSettings()
	if err != nil {
		return verifiedPR{}, errors.New("provider: dashboard settings unavailable")
	}
	allowNoCI := os.Getenv("JAWA_ALLOW_NO_CI") == "true"
	if value, ok := overrides["allow-no-ci"]; ok {
		allowNoCI = value == "true"
	}
	p.allowNoCI = allowNoCI
	var pr providerPR
	if err = p.read(t.path, &pr); err != nil {
		return verifiedPR{}, err
	}
	if err = pr.matches(t.repo, t.base, t.branch, t.number); err != nil {
		return verifiedPR{}, err
	}
	sha := pr.Head.SHA
	if pr.Merged {
		// A provider-confirmed merge is a terminal outcome; open-PR CI/review
		// gates no longer apply. Identity is still checked and re-read below.
	} else if p.forgejo {
		if err = p.forgejoCI(t.root, t.base, sha); err != nil {
			return verifiedPR{}, err
		}
		if err = p.forgejoReviews(t.path); err != nil {
			return verifiedPR{}, err
		}
	} else {
		if err = p.githubCI(t.root, t.base, sha); err != nil {
			return verifiedPR{}, err
		}
		owner, name, _ := strings.Cut(t.repo, "/")
		if err = p.githubReviews(t.path, owner, name, t.number, sha); err != nil {
			return verifiedPR{}, err
		}
	}
	// Close the common head-update race: all evidence above was for this SHA.
	var latest providerPR
	if err = p.read(t.path, &latest); err != nil {
		return verifiedPR{}, err
	}
	if err = latest.matches(t.repo, t.base, t.branch, t.number); err != nil {
		return verifiedPR{}, err
	}
	if latest.Head.SHA != sha {
		return verifiedPR{}, errors.New("provider: PR head changed during verification")
	}
	if pr.Merged && !latest.Merged {
		return verifiedPR{}, errors.New("provider: PR merge state changed during verification")
	}
	return verifiedPR{reader: p, path: t.path, repository: t.repo, base: t.base, branch: t.branch, sha: sha, number: t.number, merged: latest.Merged}, nil
}

type providerTargetInfo struct {
	reader             providerReader
	root, path         string
	repo, base, branch string
	number             int
}

// providerTarget validates immutable assignment and configured repository before any request.
func providerTarget(ctx context.Context, s *Store, c *http.Client, a Attempt) (providerTargetInfo, error) {
	if s == nil {
		return providerTargetInfo{}, errors.New("provider: store required")
	}
	overrides, err := s.providerSettings()
	if err != nil {
		return providerTargetInfo{}, errors.New("provider: dashboard settings unavailable")
	}
	var provider, repo, base string
	if err := s.db.QueryRowContext(ctx, `SELECT p.provider,p.repo,p.base_branch FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, a.CardID).Scan(&provider, &repo, &base); err != nil {
		return providerTargetInfo{}, errors.New("provider: project unavailable")
	}
	var task client.Task
	if json.Unmarshal([]byte(a.TaskJSON), &task) != nil || task.TaskID != a.TaskID || a.TaskID == "" {
		return providerTargetInfo{}, errors.New("provider: invalid immutable task")
	}
	// Parse the fixed trailing workflow fields, not user-controlled title/description.
	tail := "\nRepository: " + repo + "\nBase branch: " + base + "\nProvider: " + provider + "\nIssue: "
	pos := strings.LastIndex(task.Text, tail)
	if pos < 0 {
		return providerTargetInfo{}, errors.New("provider: task/project configuration mismatch")
	}
	if a.RepairPRURL != "" && a.PRURL != a.RepairPRURL {
		return providerTargetInfo{}, errors.New("provider: repair PR mismatch")
	}
	branch, branchErr := assignedBranch(a)
	if branchErr != nil {
		return providerTargetInfo{}, fmt.Errorf("provider: %w", branchErr)
	}
	endpoint, err := resolveProviderEndpoint(provider, repo, overrides)
	if err != nil {
		return providerTargetInfo{}, fmt.Errorf("provider: %w", err)
	}
	host, owner, name := endpoint.Host, endpoint.Owner, endpoint.Name
	u, err := url.Parse(a.PRURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host != host || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return providerTargetInfo{}, errors.New("provider: PR URL does not match configured host")
	}
	kind := endpoint.PullKind
	prefix := "/" + owner + "/" + name + "/" + kind + "/"
	if !strings.HasPrefix(u.Path, prefix) {
		return providerTargetInfo{}, errors.New("provider: PR repository mismatch")
	}
	number, err := strconv.Atoi(strings.TrimPrefix(u.Path, prefix))
	if err != nil || number < 1 || u.Path != prefix+strconv.Itoa(number) {
		return providerTargetInfo{}, errors.New("provider: invalid PR number")
	}
	if err := endpoint.requireCredentials(); err != nil {
		return providerTargetInfo{}, fmt.Errorf("provider: %w", err)
	}
	p := providerReader{ctx: ctx, client: c, token: endpoint.Token, api: endpoint.API, forgejo: endpoint.Forgejo, authPrefix: endpoint.AuthPrefix}
	root := strings.TrimPrefix(endpoint.repoAPI(), endpoint.API)
	path := root + "/pulls/" + strconv.Itoa(number)
	return providerTargetInfo{reader: p, root: root, path: path, repo: owner + "/" + name, base: base, branch: branch, number: number}, nil
}

var providerPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var providerSHA = regexp.MustCompile(`^[a-fA-F0-9]{40,64}$`)

func providerRepository(raw string) (host, owner, name string, err error) {
	if scpRepo.MatchString(raw) {
		raw = "ssh://" + strings.Replace(raw, ":", "/", 1)
	}
	u, e := url.Parse(raw)
	if strings.ContainsAny(raw, " \t\r\n\x00") || e != nil || u.ForceQuery || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != "git") || (u.User != nil && (u.Scheme != "ssh" || strings.Contains(u.User.String(), ":"))) {
		return "", "", "", errors.New("provider: unsupported repository URL: use HTTPS (or SSH/git identity) with root-path owner/repo, without credentials, query or fragment")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", "", errors.New("provider: repository must be root-path owner/name; Forgejo deployment subpaths are unsupported")
	}
	owner, name = parts[0], strings.TrimSuffix(parts[1], ".git")
	if !providerPart.MatchString(owner) || !providerPart.MatchString(name) || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", "", errors.New("provider: unsafe repository path")
	}
	return u.Host, owner, name, nil
}

type providerPR struct {
	Number         int
	State          string
	Draft          bool
	Merged         bool
	MergeableState string `json:"mergeable_state"`
	Mergeable      *bool
	Head           struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
	Base struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
}

func (p providerPR) identityMatches(repo, base, branch string, number int) bool {
	return p.Number == number && p.Base.Ref == base && p.Base.Repo.FullName == repo && p.Head.Repo.FullName == repo && p.Head.Ref == branch && providerSHA.MatchString(p.Head.SHA)
}

func (p providerPR) matches(repo, base, branch string, number int) error {
	identityOK := p.identityMatches(repo, base, branch, number)
	mergedOK := p.Merged && p.State == "closed"
	openOK := !p.Merged && p.State == "open" && !p.Draft && p.Mergeable != nil && *p.Mergeable
	if !identityOK || (!mergedOK && !openOK) {
		return errors.New("provider: PR identity, branch, head or mergeability not ready")
	}
	return nil
}

type providerReader struct {
	ctx        context.Context
	client     *http.Client
	token, api string
	authPrefix string
	forgejo    bool
	allowNoCI  bool
}

func (p providerReader) read(path string, out any) error {
	return p.request("GET", p.api+path, nil, out)
}

// do sends one authenticated request and caps its body at 4 MiB. Callers own
// status mapping; neither response bodies nor transport diagnostics are surfaced.
func (p providerReader) do(method, target string, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(p.ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, errors.New("provider: invalid request")
	}
	req.Header.Set("Authorization", p.authPrefix+p.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if p.ctx.Err() != nil {
			return 0, nil, nil, p.ctx.Err()
		}
		return 0, nil, nil, errors.New("provider: HTTP request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return resp.StatusCode, resp.Header, nil, errors.New("provider: response exceeds limit or unreadable")
	}
	return resp.StatusCode, resp.Header, data, nil
}

func (p providerReader) request(method, target string, body []byte, out any) error {
	status, header, data, err := p.do(method, target, body)
	if status != 0 && status != http.StatusOK {
		return providerHTTPError(status)
	}
	// Never follow provider-supplied pagination URLs (or silently accept truncated evidence).
	if strings.Contains(header.Get("Link"), `rel="next"`) {
		return errors.New("provider: paginated evidence exceeds supported limit")
	}
	if err != nil {
		return err
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("provider: malformed API response")
	}
	return nil
}

type providerHTTPError int

func (e providerHTTPError) Error() string { return fmt.Sprintf("provider: API returned HTTP %d", e) }

type providerStatus struct{ Context, State string }

func (p providerReader) githubCI(root, base, sha string) error {
	var branch struct{ Protected *bool }
	if err := p.read(root+"/branches/"+url.PathEscape(base), &branch); err != nil {
		return err
	}
	if branch.Protected == nil {
		return errors.New("provider: branch protection unknown")
	}
	var required struct {
		Contexts *[]string
		Checks   *[]struct {
			Context string
			AppID   int `json:"app_id"`
		}
	}
	if *branch.Protected {
		if err := p.read(root+"/branches/"+url.PathEscape(base)+"/protection/required_status_checks", &required); err != nil {
			// Ruleset-only protection has no classic required-status policy.
			if err != providerHTTPError(http.StatusNotFound) {
				return err
			}
		} else if required.Contexts == nil || required.Checks == nil {
			return errors.New("provider: required status policy incomplete")
		}
	}
	// Effective branch rules include repository and organization rulesets.
	var rules []struct {
		Type       string
		Parameters *struct {
			Required *[]struct {
				Context string
				AppID   *int `json:"integration_id"`
			} `json:"required_status_checks"`
		}
	}
	if err := p.read(root+"/rules/branches/"+url.PathEscape(base)+"?per_page=100", &rules); err != nil {
		return err
	}
	if rules == nil || len(rules) >= 100 {
		return errors.New("provider: effective rules incomplete")
	}
	type requirement struct {
		name string
		app  int
	}
	var requirements []requirement
	if required.Contexts != nil {
		for _, name := range *required.Contexts {
			requirements = append(requirements, requirement{name, 0})
		}
	}
	if required.Checks != nil {
		for _, c := range *required.Checks {
			requirements = append(requirements, requirement{c.Context, c.AppID})
		}
	}
	for _, r := range rules {
		if r.Type == "" {
			return errors.New("provider: effective rule type missing")
		}
		if r.Type != "required_status_checks" {
			continue
		} // Approvals and other merge gates remain human responsibilities.
		if r.Parameters == nil || r.Parameters.Required == nil {
			return errors.New("provider: ruleset required status policy incomplete")
		}
		for _, c := range *r.Parameters.Required {
			app := 0
			if c.AppID != nil {
				app = *c.AppID
			}
			requirements = append(requirements, requirement{c.Context, app})
		}
	}
	var checks struct {
		Total int `json:"total_count"`
		Runs  []struct {
			Name, Status, Conclusion string
			HeadSHA                  string `json:"head_sha"`
			App                      struct{ ID int }
		} `json:"check_runs"`
	}
	if err := p.read(root+"/commits/"+sha+"/check-runs?per_page=100&filter=latest", &checks); err != nil {
		return err
	}
	if checks.Runs == nil || checks.Total != len(checks.Runs) || checks.Total > 100 {
		return errors.New("provider: incomplete check runs")
	}
	passed := map[string]bool{}
	allSuccessful := true
	apps := map[string]map[int]bool{}
	for _, c := range checks.Runs {
		if c.HeadSHA != sha || c.Name == "" {
			return errors.New("provider: CI checks not completed successfully on current head")
		}
		success := c.Status == "completed" && (c.Conclusion == "success" || c.Conclusion == "skipped")
		allSuccessful = allSuccessful && success
		passed[c.Name] = passed[c.Name] || success
		if apps[c.Name] == nil {
			apps[c.Name] = map[int]bool{}
		}
		apps[c.Name][c.App.ID] = success
	}
	var status struct {
		State, SHA string
		Total      int `json:"total_count"`
		Statuses   []providerStatus
	}
	if err := p.read(root+"/commits/"+sha+"/status?per_page=100", &status); err != nil {
		return err
	}
	if status.Statuses == nil || status.SHA != sha || status.Total != len(status.Statuses) || status.Total > 100 {
		return errors.New("provider: incomplete commit statuses")
	}
	for _, s := range status.Statuses {
		if s.Context == "" || s.State == "" {
			return errors.New("provider: commit status not successful")
		}
		allSuccessful = allSuccessful && s.State == "success"
		passed[s.Context] = passed[s.Context] || s.State == "success"
	}
	for _, c := range requirements {
		if c.name == "" || !passed[c.name] || (c.app > 0 && !apps[c.name][c.app]) {
			return errors.New("provider: required CI check missing or unsuccessful")
		}
	}
	if len(requirements) == 0 && (!allSuccessful || (len(status.Statuses) > 0 && status.State != "success")) {
		return errors.New("provider: CI not successful")
	}
	if len(passed) == 0 && !p.allowNoCI {
		return errors.New("provider: no CI evidence; configure CI or explicitly set JAWA_ALLOW_NO_CI=true")
	}
	return nil
}
func (p providerReader) githubReviews(path, owner, name string, number int, sha string) error {
	var reviews []struct {
		State string
		User  struct{ ID int }
	}
	if err := p.read(path+"/reviews?per_page=100", &reviews); err != nil {
		return err
	}
	if reviews == nil || len(reviews) >= 100 {
		return errors.New("provider: review history exceeds supported limit")
	}
	latest := map[int]string{}
	for _, r := range reviews {
		if r.User.ID == 0 {
			return errors.New("provider: review identity missing")
		}
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
			latest[r.User.ID] = r.State
		case "COMMENTED":
		case "PENDING": // Unsubmitted reviews are not outstanding change requests.
		default:
			return errors.New("provider: unknown review state")
		}
	}
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return errors.New("provider: changes requested")
		}
	}
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){headRefOid reviewThreads(first:100){nodes{isResolved} pageInfo{hasNextPage}}}}}`
	body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"owner": owner, "name": name, "number": number}})
	var result struct {
		Errors []json.RawMessage
		Data   struct {
			Repository *struct {
				PullRequest *struct {
					Head    string `json:"headRefOid"`
					Threads *struct {
						Nodes *[]struct {
							Resolved *bool `json:"isResolved"`
						}
						PageInfo *struct {
							Next *bool `json:"hasNextPage"`
						}
					} `json:"reviewThreads"`
				}
			}
		}
	}
	if err := p.request("POST", p.api+"/graphql", body, &result); err != nil {
		return err
	}
	if len(result.Errors) > 0 || result.Data.Repository == nil || result.Data.Repository.PullRequest == nil {
		return errors.New("provider: review threads unavailable")
	}
	pr := result.Data.Repository.PullRequest
	if pr.Head != sha || pr.Threads == nil || pr.Threads.Nodes == nil || pr.Threads.PageInfo == nil || pr.Threads.PageInfo.Next == nil || *pr.Threads.PageInfo.Next {
		return errors.New("provider: incomplete or stale review threads")
	}
	for _, t := range *pr.Threads.Nodes {
		if t.Resolved == nil || !*t.Resolved {
			return errors.New("provider: unresolved review thread")
		}
	}
	return nil
}

// Forgejo's branch endpoint reports the effective protection, including wildcard
// rules; looking up a protection by the literal base name would miss those rules.
func (p providerReader) forgejoCI(root, base, sha string) error {
	var branch struct {
		Name      string
		Protected *bool
		Enabled   *bool     `json:"enable_status_check"`
		Contexts  *[]string `json:"status_check_contexts"`
	}
	if err := p.read(root+"/branches/"+url.PathEscape(base), &branch); err != nil {
		return err
	}
	if branch.Name != base || branch.Protected == nil || branch.Enabled == nil || (*branch.Enabled && branch.Contexts == nil) {
		return errors.New("provider: Forgejo effective status policy incomplete")
	}
	var status struct {
		SHA, State string
		Total      *int `json:"total_count"`
		Statuses   []struct {
			Context string
			State   string `json:"status"`
		}
	}
	if err := p.read(root+"/commits/"+sha+"/status?limit=100&page=1", &status); err != nil {
		return err
	}
	if status.SHA != sha || status.Statuses == nil || status.Total == nil || *status.Total != len(status.Statuses) || len(status.Statuses) >= 100 {
		return errors.New("provider: Forgejo commit statuses incomplete or stale")
	}
	passed := map[string]bool{}
	allSuccessful := true
	for _, s := range status.Statuses {
		if s.Context == "" || s.State == "" {
			return errors.New("provider: Forgejo commit status incomplete")
		}
		passed[s.Context] = s.State == "success"
		allSuccessful = allSuccessful && s.State == "success"
	}
	required := []string{}
	if *branch.Enabled {
		required = *branch.Contexts
	}
	for _, name := range required {
		if name == "" || !passed[name] {
			return errors.New("provider: Forgejo required CI context missing or unsuccessful")
		}
	}
	if len(status.Statuses) == 0 {
		return errors.New("provider: Forgejo CI absent")
	}
	if len(required) == 0 && (!allSuccessful || status.State != "success") {
		return errors.New("provider: Forgejo CI unsuccessful")
	}
	return nil
}

func (p providerReader) forgejoReviews(path string) error {
	var reviews []struct {
		ID        int
		State     string
		Dismissed *bool
		Count     *int `json:"comments_count"`
		User      struct{ ID int }
	}
	if err := p.read(path+"/reviews?limit=100&page=1", &reviews); err != nil {
		return err
	}
	if reviews == nil || len(reviews) >= 100 {
		return errors.New("provider: Forgejo review history incomplete")
	}
	for _, r := range reviews {
		if r.ID <= 0 || r.User.ID <= 0 || r.Dismissed == nil || r.Count == nil || *r.Count < 0 {
			return errors.New("provider: Forgejo review incomplete")
		}
		// Dismissal clears this formal request, not its unresolved inline threads.
		switch r.State {
		case "REQUEST_CHANGES":
			if !*r.Dismissed {
				return errors.New("provider: Forgejo changes requested")
			}
		case "APPROVED", "COMMENT", "PENDING", "REQUEST_REVIEW":
		default:
			return errors.New("provider: Forgejo unknown review state")
		}
		var comments []struct {
			ID       int
			ReviewID int `json:"pull_request_review_id"`
			Resolver *struct{ ID int }
		}
		if err := p.read(path+"/reviews/"+strconv.Itoa(r.ID)+"/comments", &comments); err != nil {
			return err
		}
		if comments == nil || len(comments) != *r.Count {
			return errors.New("provider: Forgejo review comments incomplete")
		}
		for _, c := range comments {
			// The documented resolution signal is resolver, not an invented resolved
			// boolean. Older versions omitting it block only when comments exist.
			if c.ID <= 0 || c.ReviewID != r.ID || c.Resolver == nil || c.Resolver.ID <= 0 {
				return errors.New("provider: Forgejo review comment unresolved or resolution unavailable")
			}
		}
	}
	return nil
}

// ProviderIdentity reads only PR identity, deliberately not readiness evidence.
func ProviderIdentity(s *Store, supplied *http.Client) IdentityChecker {
	c := providerHTTPClient(supplied)
	return func(ctx context.Context, a Attempt, selected string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		a.PRURL = selected
		t, err := providerTarget(ctx, s, c, a)
		if err != nil {
			return "", err
		}
		p := t.reader
		var pr providerPR
		if err = p.read(t.path, &pr); err != nil {
			return "", err
		}
		if !pr.identityMatches(t.repo, t.base, t.branch, t.number) || (pr.State != "open" || pr.Merged) && (pr.State != "closed" || !pr.Merged) {
			return "", errors.New("provider: PR identity or state invalid")
		}
		return pr.Head.SHA, nil
	}
}
