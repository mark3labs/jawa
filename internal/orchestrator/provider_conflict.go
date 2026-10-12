package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// PRCheck is current provider evidence, not a change to historical readiness.
// Only State == "conflict" is a confirmed merge conflict. Unknown and error
// must never be rendered as conflicts. CheckedAt lets callers show freshness.
type PRCheck struct {
	AttemptID, PRURL, State, HeadSHA, Error string
	CheckedAt                               time.Time
}

type ConflictChecker func(context.Context, Attempt) (PRCheck, error)

// ProviderConflictChecker reads only PR metadata (at most two bounded requests).
// GitHub's dirty state distinguishes conflicts from other non-mergeable gates.
func ProviderConflictChecker(s *Store, supplied *http.Client) ConflictChecker {
	c := http.Client{}
	if supplied != nil {
		c = *supplied
	}
	c.Timeout = 20 * time.Second
	c.Jar = nil
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context, a Attempt) (result PRCheck, checkErr error) {
		defer func() {
			result.CheckedAt = time.Now().UTC()
			if checkErr != nil {
				result.State, result.HeadSHA, result.Error = "error", "", "provider check failed"
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		result = PRCheck{AttemptID: a.ID, PRURL: a.PRURL, State: "unknown"}
		p, _, path, owner, name, base, number, err := providerTarget(ctx, s, &c, a)
		if err != nil {
			return result, err
		}
		branch, err := assignedBranch(a)
		if err != nil {
			return result, err
		}
		var pr providerPR
		if err = p.read(path, &pr); err != nil {
			return result, err
		}
		if !pr.identityMatches(owner+"/"+name, base, branch, number) {
			return result, errors.New("provider: PR identity mismatch")
		}
		result.HeadSHA = pr.Head.SHA
		result.State = pr.conflictState(p.forgejo)
		if result.State != "conflict" {
			return result, nil
		}
		// Confirm on the same head; don't persist transient or stale conflict evidence.
		var latest providerPR
		if err = p.read(path, &latest); err != nil {
			return result, err
		}
		if !latest.identityMatches(owner+"/"+name, base, branch, number) {
			return result, errors.New("provider: PR identity mismatch")
		}
		if latest.Head.SHA != pr.Head.SHA || latest.conflictState(p.forgejo) != "conflict" {
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
