package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Each phase is separately bounded. No write is internally retried.
type mergeAdapter struct {
	store  *Store
	client *http.Client
}
type autoMergeOutcome struct {
	state, category string
	retryAfter      time.Time
}

func newMergeAdapter(s *Store, c *http.Client) *mergeAdapter {
	return &mergeAdapter{s, providerHTTPClient(c)}
}
func (m *mergeAdapter) prepare(ctx context.Context, a Attempt) (verifiedPR, error) {
	return verifyProviderPR(ctx, m.store, m.client, a)
}
func (m *mergeAdapter) confirm(ctx context.Context, a Attempt, sha string) autoMergeOutcome {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p, _, path, owner, name, base, number, err := providerTarget(ctx, m.store, m.client, a)
	if err != nil {
		return autoMergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	branch, err := assignedBranch(a)
	if err != nil {
		return autoMergeOutcome{state: "uncertain", category: "identity_changed"}
	}
	var pr providerPR
	if p.read(path, &pr) != nil || !pr.checkIdentityMatches(owner+"/"+name, base, branch, number) || pr.Head.SHA != sha {
		return autoMergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	if pr.Merged && pr.State == "closed" {
		var latest providerPR
		if p.read(path, &latest) == nil && latest.checkIdentityMatches(owner+"/"+name, base, branch, number) && latest.Head.SHA == sha && latest.Merged && latest.State == "closed" {
			return autoMergeOutcome{state: "merged"}
		}
		return autoMergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	if !pr.Merged && pr.State == "open" {
		return autoMergeOutcome{state: "deferred", category: "confirmed_open"}
	}
	return autoMergeOutcome{state: "uncertain", category: "closed_unmerged"}
}
func (m *mergeAdapter) dispatch(ctx context.Context, a Attempt, v verifiedPR) autoMergeOutcome {
	phase, cancel := context.WithTimeout(ctx, 20*time.Second)
	// Re-resolve credentials and assignment, never carry credentials in the ledger.
	p, _, path, _, _, _, _, err := providerTarget(phase, m.store, m.client, a)
	if err != nil || path != v.path || p.api != v.reader.api || p.forgejo != v.reader.forgejo {
		cancel()
		return autoMergeOutcome{state: "deferred", category: "configuration_unavailable"}
	}
	method := http.MethodPut
	payload := map[string]any{"sha": v.sha, "merge_method": "squash"}
	if p.forgejo {
		method = http.MethodPost
		payload = map[string]any{"Do": "squash", "head_commit_id": v.sha, "force_merge": false, "merge_when_checks_succeed": false, "delete_branch_after_merge": false}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(phase, method, p.api+path+"/merge", bytes.NewReader(body))
	if err != nil {
		cancel()
		return autoMergeOutcome{state: "deferred", category: "configuration_unavailable"}
	}
	req.Header.Set("Authorization", p.authPrefix+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	outcome := autoMergeOutcome{state: "uncertain", category: "delivery_uncertain"}
	if err == nil {
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		_ = resp.Body.Close()
		if readErr == nil && len(raw) <= 4<<20 {
			switch resp.StatusCode {
			case http.StatusOK:
				if p.forgejo {
					outcome.category = "confirmation_pending"
				} else {
					var result struct {
						Merged bool
						SHA    string
					}
					if json.Unmarshal(raw, &result) == nil && result.Merged && providerSHA.MatchString(result.SHA) {
						outcome.category = "confirmation_pending"
					}
				}
			case http.StatusUnauthorized, http.StatusForbidden:
				outcome = autoMergeOutcome{state: "rejected", category: "permissions_or_protection"}
			case http.StatusMethodNotAllowed:
				outcome = autoMergeOutcome{state: "rejected", category: "squash_or_protection"}
			case http.StatusConflict:
				outcome = autoMergeOutcome{state: "deferred", category: "sha_conflict"}
			case http.StatusUnprocessableEntity:
				outcome = autoMergeOutcome{state: "rejected", category: "protection_pending"}
			case http.StatusTooManyRequests:
				outcome = autoMergeOutcome{state: "deferred", category: "rate_limited"}
			}
			if resp.StatusCode == 429 || resp.StatusCode == 403 {
				if seconds, e := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 32); e == nil && seconds > 0 {
					outcome.retryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
				} else if t, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
					outcome.retryAfter = t
				}
				if resp.StatusCode == http.StatusForbidden && !outcome.retryAfter.IsZero() {
					outcome.state, outcome.category = "deferred", "rate_limited"
				}
			}
		}
	}
	cancel()
	if outcome.state == "uncertain" {
		confirmed := m.confirm(ctx, a, v.sha)
		if confirmed.state == "merged" || confirmed.state == "deferred" {
			return confirmed
		}
	}
	return outcome
}

// A merger reports only sanitized states. Once a write starts, anything other
// than a definite refusal needs read-only reconciliation, never a write retry.
type ProviderMerger func(context.Context, Attempt, string, string) string

func validMergeMethod(method string) bool {
	return method == "merge" || method == "squash" || method == "rebase"
}

func ProviderPRMerger(s *Store, supplied *http.Client) ProviderMerger {
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout, c.Jar = 20*time.Second, nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context, a Attempt, head, method string) string {
		if !validMergeMethod(method) || !providerSHA.MatchString(head) {
			return "rejected"
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		p, _, path, owner, name, base, number, err := providerTarget(ctx, s, &c, a)
		if err != nil {
			return "rejected"
		}
		branch, err := assignedBranch(a)
		if err != nil {
			return "rejected"
		}
		var pr providerPR
		if p.read(path, &pr) != nil || !pr.checkIdentityMatches(owner+"/"+name, base, branch, number) || pr.Head.SHA != head {
			return "rejected"
		}
		if pr.conflictState(p.forgejo) == "merged" {
			return "merged"
		}
		if pr.conflictState(p.forgejo) != "clear" {
			return "rejected"
		}
		verb := http.MethodPut
		payload := map[string]any{"sha": head, "merge_method": method}
		if p.forgejo {
			verb = http.MethodPost
			payload = map[string]any{"Do": method, "head_commit_id": head, "force_merge": false, "merge_when_checks_succeed": false, "delete_branch_after_merge": false}
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, verb, p.api+path+"/merge", bytes.NewReader(body))
		if err != nil {
			return "rejected"
		}
		req.Header.Set("Authorization", p.authPrefix+p.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")
		// Exactly one write attempt. Do not use the read parser: Forgejo's documented
		// successful response has no JSON body. Redirects are never followed.
		resp, err := c.Do(req)
		if err != nil {
			return "uncertain"
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			switch resp.StatusCode {
			case 401, 403, 404, 405, 409, 422:
				return "rejected"
			default:
				return "uncertain"
			}
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
		if err != nil || len(data) > 4<<20 {
			return "uncertain"
		}
		if !p.forgejo {
			var result struct {
				Merged *bool `json:"merged"`
			}
			if json.Unmarshal(data, &result) != nil || result.Merged == nil {
				return "uncertain"
			}
			if !*result.Merged {
				return "rejected"
			}
		} else if len(bytes.TrimSpace(data)) != 0 {
			// Some hosts send JSON; reject malformed success rather than guess.
			if !json.Valid(data) {
				return "uncertain"
			}
		}
		var confirmed providerPR
		if p.read(path, &confirmed) != nil || !confirmed.checkIdentityMatches(owner+"/"+name, base, branch, number) || confirmed.Head.SHA != head || confirmed.conflictState(p.forgejo) != "merged" {
			return "uncertain"
		}
		return "merged"
	}
}
