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

// ProviderVerifier independently reads provider state on every invocation. The
// supplied client supplies transport only; redirects and cookies are disabled.
func ProviderVerifier(s *Store, supplied *http.Client) Verifier {
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout = 20 * time.Second
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context, a Attempt) (bool, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		if s == nil {
			return false, errors.New("provider: store required")
		}
		var provider, repo, base string
		if err := s.db.QueryRowContext(ctx, `SELECT p.provider,p.repo,p.base_branch FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, a.CardID).Scan(&provider, &repo, &base); err != nil {
			return false, errors.New("provider: project unavailable")
		}
		var task client.Task
		if json.Unmarshal([]byte(a.TaskJSON), &task) != nil || task.TaskID != a.TaskID || a.TaskID == "" {
			return false, errors.New("provider: invalid immutable task")
		}
		// Parse the fixed trailing workflow fields, not user-controlled title/description.
		tail := "\nRepository: " + repo + "\nBase branch: " + base + "\nProvider: " + provider + "\nIssue: "
		pos := strings.LastIndex(task.Text, tail)
		if pos < 0 {
			return false, errors.New("provider: task/project configuration mismatch")
		}
		expected := fmt.Sprintf("jawa/card/%s/attempt/%d", a.CardID, a.Number)
		// Legacy workflow tasks do not contain this instruction. Never guess a branch.
		if a.Number < 1 || strings.Count(task.Text, "\nBranch: ") != 1 || !strings.HasSuffix(task.Text, "\nBranch: "+expected) {
			return false, errors.New("provider: task lacks deterministic Branch instruction")
		}
		host, owner, name, err := providerRepository(repo)
		if err != nil {
			return false, err
		}
		u, err := url.Parse(a.PRURL)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host != host || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
			return false, errors.New("provider: PR URL does not match configured host")
		}
		kind := "pull"
		if provider == "forgejo" {
			kind = "pulls"
		}
		prefix := "/" + owner + "/" + name + "/" + kind + "/"
		if !strings.HasPrefix(u.Path, prefix) {
			return false, errors.New("provider: PR repository mismatch")
		}
		number, err := strconv.Atoi(strings.TrimPrefix(u.Path, prefix))
		if err != nil || number < 1 || u.Path != prefix+strconv.Itoa(number) {
			return false, errors.New("provider: invalid PR number")
		}
		token, api := "", ""
		switch provider {
		case "github":
			if host != "github.com" {
				return false, errors.New("provider: only github.com is supported")
			}
			token = os.Getenv("JAWA_GITHUB_TOKEN")
			if token == "" {
				token = os.Getenv("GITHUB_TOKEN")
			}
			api = "https://api.github.com"
		case "forgejo":
			token = os.Getenv("JAWA_FORGEJO_TOKEN")
			api = "https://" + host + "/api/v1"
		default:
			return false, errors.New("provider: unsupported provider")
		}
		if strings.TrimSpace(token) == "" {
			return false, errors.New("provider: credentials missing")
		}
		p := providerReader{ctx: ctx, client: &c, token: token, api: api, forgejo: provider == "forgejo"}
		root := "/repos/" + owner + "/" + name
		path := root + "/pulls/" + strconv.Itoa(number)
		var pr providerPR
		if err = p.read(path, &pr); err != nil {
			return false, err
		}
		if err = pr.matches(owner+"/"+name, base, expected, number); err != nil {
			return false, err
		}
		sha := pr.Head.SHA
		if provider == "forgejo" {
			if err = p.forgejoCI(root, base, sha); err != nil {
				return false, err
			}
			if err = p.forgejoReviews(path); err != nil {
				return false, err
			}
		} else {
			if err = p.githubCI(root, base, sha); err != nil {
				return false, err
			}
			if err = p.githubReviews(root, path, owner, name, number, sha); err != nil {
				return false, err
			}
		}
		// Close the common head-update race: all evidence above was for this SHA.
		var latest providerPR
		if err = p.read(path, &latest); err != nil {
			return false, err
		}
		if err = latest.matches(owner+"/"+name, base, expected, number); err != nil {
			return false, err
		}
		if latest.Head.SHA != sha {
			return false, errors.New("provider: PR head changed during verification")
		}
		return true, nil
	}
}

var providerPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var providerSHA = regexp.MustCompile(`^[a-fA-F0-9]{40,64}$`)

func providerRepository(raw string) (host, owner, name string, err error) {
	if scpRepo.MatchString(raw) {
		raw = "ssh://" + strings.Replace(raw, ":", "/", 1)
	}
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || (u.Scheme != "https" && u.Scheme != "ssh" && u.Scheme != "git") || (u.User != nil && (u.Scheme != "ssh" || strings.Contains(u.User.String(), ":"))) {
		return "", "", "", errors.New("provider: unsafe repository URL")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", "", errors.New("provider: repository must be owner/name")
	}
	owner, name = parts[0], strings.TrimSuffix(parts[1], ".git")
	if !providerPart.MatchString(owner) || !providerPart.MatchString(name) || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", "", "", errors.New("provider: unsafe repository path")
	}
	return u.Host, owner, name, nil
}

type providerPR struct {
	Number    int
	State     string
	Draft     bool
	Merged    bool
	Mergeable *bool
	Head      struct {
		Ref, SHA string
		Repo     struct {
			FullName string `json:"full_name"`
		}
	}
	Base struct {
		Ref  string
		Repo struct {
			FullName string `json:"full_name"`
		}
	}
}

func (p providerPR) matches(repo, base, branch string, number int) error {
	if p.Number != number || p.State != "open" || p.Draft || p.Merged || p.Mergeable == nil || !*p.Mergeable || p.Base.Ref != base || p.Base.Repo.FullName != repo || p.Head.Repo.FullName != repo || p.Head.Ref != branch || !providerSHA.MatchString(p.Head.SHA) {
		return errors.New("provider: PR identity, branch, head or mergeability not ready")
	}
	return nil
}

type providerReader struct {
	ctx        context.Context
	client     *http.Client
	token, api string
	forgejo    bool
}

func (p providerReader) read(path string, out any) error {
	return p.request("GET", p.api+path, nil, out)
}
func (p providerReader) request(method, target string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(p.ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return errors.New("provider: invalid request")
	}
	auth := "Bearer "
	if p.forgejo {
		auth = "token "
	}
	req.Header.Set("Authorization", auth+p.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		if p.ctx.Err() != nil {
			return p.ctx.Err()
		}
		return errors.New("provider: HTTP request failed")
	}
	defer func() { _ = resp.Body.Close() }() // Best-effort transport cleanup.
	if resp.StatusCode != http.StatusOK {
		return providerHTTPError(resp.StatusCode)
	}
	// Never follow provider-supplied pagination URLs (or silently accept truncated evidence).
	if strings.Contains(resp.Header.Get("Link"), `rel="next"`) {
		return errors.New("provider: paginated evidence exceeds supported limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil || len(data) > 4<<20 {
		return errors.New("provider: response exceeds limit or unreadable")
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
		success := c.Status == "completed" && c.Conclusion == "success"
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
	if len(passed) == 0 && os.Getenv("JAWA_ALLOW_NO_CI") != "true" {
		return errors.New("provider: no CI evidence; configure CI or explicitly set JAWA_ALLOW_NO_CI=true")
	}
	return nil
}
func (p providerReader) githubReviews(root, path, owner, name string, number int, sha string) error {
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
