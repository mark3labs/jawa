package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// PRCheck is effective display status, not a change to historical readiness.
// Manual markers have their own timestamp and never fabricate provider evidence.
// Only State == "conflict" is a confirmed merge conflict. Unknown and error
// must never be rendered as conflicts. CheckedAt lets callers show freshness.
type PRCheck struct {
	AttemptID, PRURL, State, HeadSHA, BaseSHA, Error string
	CheckedAt                                        time.Time
	Manual                                           bool
	Open                                             bool
	MarkedAt                                         time.Time
}

type ConflictChecker func(context.Context, Attempt) (PRCheck, error)

// ProviderConflictChecker reads only PR metadata (at most two bounded requests).
// GitHub's dirty state distinguishes conflicts from other non-mergeable gates.
func ProviderConflictChecker(s *Store, supplied *http.Client) ConflictChecker {
	c := providerHTTPClient(supplied)
	return func(ctx context.Context, a Attempt) (result PRCheck, checkErr error) {
		defer func() {
			result.CheckedAt = time.Now().UTC()
			if checkErr != nil {
				result.Open = false
				result.State, result.HeadSHA, result.BaseSHA, result.Error = "error", "", "", "provider check failed"
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		result = PRCheck{AttemptID: a.ID, PRURL: a.PRURL, State: "unknown"}
		t, err := providerTarget(ctx, s, c, a)
		if err != nil {
			return result, err
		}
		p := t.reader
		var pr providerPR
		if err = p.read(t.path, &pr); err != nil {
			return result, err
		}
		if !pr.checkIdentityMatches(t.repo, t.base, t.branch, t.number) {
			return result, errors.New("provider: PR identity mismatch")
		}
		result.Open = pr.State == "open" && !pr.Merged
		result.HeadSHA, result.BaseSHA = pr.Head.SHA, pr.Base.SHA
		result.State = pr.conflictState(p.forgejo)
		if result.State != "conflict" && result.State != "clear" && result.State != "merged" {
			return result, nil
		}
		// Confirm both sides of the merge; a moving base can invalidate a clear too.
		if result.State != "merged" && !providerSHA.MatchString(pr.Base.SHA) {
			result.State = "unknown"
			return result, nil
		}
		var latest providerPR
		if err = p.read(t.path, &latest); err != nil {
			return result, err
		}
		result.Open = latest.State == "open" && !latest.Merged
		if !latest.checkIdentityMatches(t.repo, t.base, t.branch, t.number) {
			return result, errors.New("provider: PR identity mismatch")
		}
		if latest.conflictState(p.forgejo) == "merged" && (result.State != "merged" || latest.Head.SHA == pr.Head.SHA) {
			result.State, result.HeadSHA, result.BaseSHA = "merged", latest.Head.SHA, latest.Base.SHA
			return result, nil
		}
		if result.State == "merged" || latest.Head.SHA != pr.Head.SHA || latest.Base.SHA != pr.Base.SHA || latest.conflictState(p.forgejo) != result.State {
			result.State = "unknown"
		}
		return result, nil
	}
}

func (p providerPR) conflictState(forgejo bool) string {
	if p.Merged && p.State == "closed" {
		return "merged"
	}
	if !p.Merged && p.State == "closed" {
		return "closed"
	}
	if p.Merged || p.State != "open" || p.Draft || p.Mergeable == nil {
		return "unknown"
	}
	if *p.Mergeable {
		return "clear"
	}
	if forgejo || p.MergeableState == "dirty" {
		return "conflict"
	}
	return "unknown"
}

// Only terminal display detection tolerates a deleted head repository. Readiness
// verification and open PR checks retain their stricter identity validator.
func (p providerPR) checkIdentityMatches(repo, base, branch string, number int) bool {
	if p.Merged && p.State == "closed" {
		return p.Number == number && p.Base.Ref == base && p.Base.Repo.FullName == repo && (p.Head.Repo.FullName == "" || p.Head.Repo.FullName == repo) && p.Head.Ref == branch && providerSHA.MatchString(p.Head.SHA)
	}
	return p.identityMatches(repo, base, branch, number)
}
