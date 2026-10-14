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
type mergeOutcome struct {
	state, category string
	retryAfter      time.Time
}

func newMergeAdapter(s *Store, c *http.Client) *mergeAdapter {
	return &mergeAdapter{s, providerHTTPClient(c)}
}
func (m *mergeAdapter) prepare(ctx context.Context, a Attempt) (verifiedPR, error) {
	return verifyProviderPR(ctx, m.store, m.client, a)
}
func (m *mergeAdapter) confirm(ctx context.Context, a Attempt, sha string) mergeOutcome {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	p, _, path, owner, name, base, number, err := providerTarget(ctx, m.store, m.client, a)
	if err != nil {
		return mergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	branch, err := assignedBranch(a)
	if err != nil {
		return mergeOutcome{state: "uncertain", category: "identity_changed"}
	}
	var pr providerPR
	if p.read(path, &pr) != nil || !pr.checkIdentityMatches(owner+"/"+name, base, branch, number) || pr.Head.SHA != sha {
		return mergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	if pr.Merged && pr.State == "closed" {
		var latest providerPR
		if p.read(path, &latest) == nil && latest.checkIdentityMatches(owner+"/"+name, base, branch, number) && latest.Head.SHA == sha && latest.Merged && latest.State == "closed" {
			return mergeOutcome{state: "merged"}
		}
		return mergeOutcome{state: "uncertain", category: "confirmation_unavailable"}
	}
	if !pr.Merged && pr.State == "open" {
		return mergeOutcome{state: "deferred", category: "confirmed_open"}
	}
	return mergeOutcome{state: "uncertain", category: "closed_unmerged"}
}
func (m *mergeAdapter) dispatch(ctx context.Context, a Attempt, v verifiedPR) mergeOutcome {
	phase, cancel := context.WithTimeout(ctx, 20*time.Second)
	// Re-resolve credentials and assignment, never carry credentials in the ledger.
	p, _, path, _, _, _, _, err := providerTarget(phase, m.store, m.client, a)
	if err != nil || path != v.path || p.api != v.reader.api || p.forgejo != v.reader.forgejo {
		cancel()
		return mergeOutcome{state: "deferred", category: "configuration_unavailable"}
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
		return mergeOutcome{state: "deferred", category: "configuration_unavailable"}
	}
	req.Header.Set("Authorization", p.authPrefix+p.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := m.client.Do(req)
	outcome := mergeOutcome{state: "uncertain", category: "delivery_uncertain"}
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
				outcome = mergeOutcome{state: "rejected", category: "permissions_or_protection"}
			case http.StatusMethodNotAllowed:
				outcome = mergeOutcome{state: "rejected", category: "squash_or_protection"}
			case http.StatusConflict:
				outcome = mergeOutcome{state: "deferred", category: "sha_conflict"}
			case http.StatusUnprocessableEntity:
				outcome = mergeOutcome{state: "rejected", category: "protection_pending"}
			case http.StatusTooManyRequests:
				outcome = mergeOutcome{state: "deferred", category: "rate_limited"}
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
