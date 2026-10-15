package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

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
