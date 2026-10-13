package orchestrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/log"
)

type IdentityChecker func(context.Context, Attempt, string) (string, error)

func (w *Workflow) SetProviderIdentity(check IdentityChecker) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.identity = check
}

type readyOverride struct {
	PRURL, HeadSHA, ActionToken, Operator, Reason string
	MarkedAt                                      time.Time
}

func (s *Store) readyOverrides() (map[string]readyOverride, error) {
	rows, err := s.db.Query(`SELECT attempt_id,pr_url,head_sha,action_token,operator,reason,marked_at FROM workflow_ready_overrides`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]readyOverride)
	for rows.Next() {
		var id string
		var r readyOverride
		var ms int64
		if err := rows.Scan(&id, &r.PRURL, &r.HeadSHA, &r.ActionToken, &r.Operator, &r.Reason, &ms); err != nil {
			return nil, err
		}
		r.MarkedAt = time.UnixMilli(ms).UTC()
		out[id] = r
	}
	return out, rows.Err()
}

func forceReadyReason(c Card, all []Attempt, pending bool) string {
	if c.Status != "Building" && c.Status != "Fix" {
		return "Force ready requires Building or Fix."
	}
	latest := latestAttempt(c.ID, all)
	if len(latest) == 0 {
		return "Force ready requires an execution attempt."
	}
	a := latest[0]
	if a.Ready || a.State == "ready" {
		return "Attempt is already ready."
	}
	for _, old := range all {
		if old.CardID == c.ID && attemptActive(old) {
			return "Active attempt — use Force stop first."
		}
	}
	if pending || a.Error == resumePendingNotice {
		return "Resume pending — use Force stop first."
	}
	if a.Error == executionConflictNotice {
		return "Reconcile the execution identity first."
	}
	return ""
}

func forceReadyToken(c Card, a Attempt, p Project) string {
	// JSON preserves field boundaries, including arbitrary user-controlled strings.
	raw, _ := json.Marshal(struct {
		ID, Lane                                   string
		Ready                                      bool
		State, RunState, Outcome                   string
		Seq                                        int
		PR, Error, RepairPR, Branch, RepairMessage string
		RepairSource                               int
		Project, Provider, Repo, Base              string
	}{a.ID, c.Status, a.Ready, a.State, a.RunState, a.OutcomeJSON, a.EventSeq, a.PRURL, a.Error, a.RepairPRURL, a.Branch, a.RepairMessage, a.RepairSourceNumber, p.ID, p.Provider, p.Repo, p.BaseBranch})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func forceReadyPR(c Card, a Attempt, p Project, number string) (string, error) {
	stored := a.PRURL
	if stored == "" {
		stored = a.RepairPRURL
	}
	if a.PRURL != "" && a.RepairPRURL != "" && a.PRURL != a.RepairPRURL {
		return "", errors.New("conflicting repair PR assignment")
	}
	var n int
	var err error
	if number != "" {
		n, err = strconv.Atoi(number)
		if err != nil || n < 1 {
			return "", errors.New("PR number must be a positive integer")
		}
	}
	if stored != "" {
		u, e := url.Parse(stored)
		if e != nil {
			return "", errors.New("invalid recorded PR")
		}
		recorded, e := strconv.Atoi(stored[strings.LastIndex(stored, "/")+1:])
		canonical, ce := projectPRURL(p.Provider, p.Repo, recorded)
		if e != nil || ce != nil || u.String() != canonical || stored != canonical {
			return "", errors.New("recorded PR does not match project")
		}
		if number != "" && n != recorded {
			return "", errors.New("PR number does not match recorded PR")
		}
	} else {
		stored, err = projectPRURL(p.Provider, p.Repo, n)
		if err != nil {
			return "", err
		}
	}
	if c.Status == "Fix" && (a.RepairPRURL == "" || stored != a.RepairPRURL) {
		return "", errors.New("the Fix status requires its assigned repair PR")
	}
	return stored, nil
}

type readyQuery interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

// Read all local authority in one snapshot. Never retain rows across provider I/O.
func readySnapshot(q readyQuery, id string) (Card, Project, []Attempt, bool, error) {
	var c Card
	var p Project
	if err := q.QueryRow(`SELECT c.id,c.status,c.project_id,p.provider,p.repo,p.base_branch FROM cards c JOIN projects p ON p.id=c.project_id WHERE c.id=?`, id).Scan(&c.ID, &c.Status, &c.ProjectID, &p.Provider, &p.Repo, &p.BaseBranch); err != nil {
		return c, p, nil, false, workflowCardError(err)
	}
	p.ID = c.ProjectID
	rows, err := q.Query(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE card_id=? ORDER BY number`, id)
	if err != nil {
		return c, p, nil, false, err
	}
	var all []Attempt
	for rows.Next() {
		a, e := scanAttempt(rows)
		if e != nil {
			_ = rows.Close()
			return c, p, nil, false, e
		}
		all = append(all, a)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return c, p, nil, false, err
	}
	var pending bool
	err = q.QueryRow(`SELECT EXISTS(SELECT 1 FROM failed_resumes WHERE card_id=? AND status='pending')`, id).Scan(&pending)
	return c, p, all, pending, err
}

func readyDuplicate(q readyQuery, c Card, all []Attempt, p Project, token string) (bool, error) {
	latest := latestAttempt(c.ID, all)
	if len(latest) == 0 {
		return false, nil
	}
	a := latest[0]
	var pr, expected, prior string
	err := q.QueryRow(`SELECT pr_url,action_token,prior_snapshot FROM workflow_ready_overrides WHERE attempt_id=?`, a.ID).Scan(&pr, &expected, &prior)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var old Attempt
	if json.Unmarshal([]byte(prior), &old) != nil {
		return false, errors.New("invalid ready audit snapshot")
	}
	previous := c
	previous.Status = "Building"
	matches := forceReadyToken(previous, old, p) == token
	previous.Status = "Fix"
	matches = matches || forceReadyToken(previous, old, p) == token
	// A replay may not substitute a different PR number.
	return matches && c.Status == "Done" && a.Ready && a.State == "ready" && a.PRURL == pr && expected == token && token != "", nil
}

func (w *Workflow) ForceReady(ctx context.Context, cardID, expected, prNumber, reason, operator string, acknowledged bool) error {
	if !acknowledged {
		return errors.New("confirm that CI, review and readiness checks are bypassed")
	}
	if !utf8.ValidString(reason) || len(reason) > 1000 {
		return errors.New("reason must be valid UTF-8 and at most 1000 bytes")
	}
	if prNumber != "" {
		n, err := strconv.Atoi(prNumber)
		if err != nil || n < 1 {
			return errors.New("PR number must be a positive integer")
		}
	}
	if strings.TrimSpace(operator) == "" {
		return errors.New("authenticated operator required")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stop := context.AfterFunc(w.ctx, cancel)
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	// A read transaction gives a coherent preflight; release it before network I/O.
	read, err := w.s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	c, p, all, pending, err := readySnapshot(read, cardID)
	if err != nil {
		_ = read.Rollback()
		return err
	}
	duplicate, err := readyDuplicate(read, c, all, p, expected)
	_ = read.Rollback()
	if err != nil {
		return err
	}
	if duplicate {
		_, err = forceReadyPR(c, latestAttempt(cardID, all)[0], p, prNumber)
		return err
	}
	if why := forceReadyReason(c, all, pending); why != "" {
		return errors.New(why)
	}
	a := latestAttempt(cardID, all)[0]
	if expected == "" || expected != forceReadyToken(c, a, p) {
		return errors.New("card changed; reopen Force ready and try again")
	}
	pr, err := forceReadyPR(c, a, p, prNumber)
	if err != nil {
		return err
	}
	w.mu.RLock()
	check := w.identity
	w.mu.RUnlock()
	if check == nil {
		return errors.New("provider identity check unavailable")
	}
	sha, err := check(ctx, a, pr)
	if err != nil {
		return err
	}
	if !providerSHA.MatchString(sha) {
		return errors.New("provider: invalid head SHA")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	tx, err := w.s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, project, history, pending, err := readySnapshot(tx, cardID)
	if err != nil {
		return err
	}
	duplicate, err = readyDuplicate(tx, current, history, project, expected)
	if err != nil {
		return err
	}
	if duplicate {
		_, err = forceReadyPR(current, latestAttempt(cardID, history)[0], project, prNumber)
		return err
	}
	if why := forceReadyReason(current, history, pending); why != "" {
		return errors.New(why)
	}
	latest := latestAttempt(cardID, history)[0]
	if forceReadyToken(current, latest, project) != expected {
		return errors.New("card changed; reopen Force ready and try again")
	}
	if err = noActiveAttempts(tx, cardID); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	prior, err := json.Marshal(latest)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	if _, err = tx.Exec(`INSERT INTO workflow_ready_overrides(attempt_id,pr_url,head_sha,action_token,prior_snapshot,operator,reason,marked_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, pr, sha, expected, string(prior), operator, reason, now); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE workflow_attempts SET ready=1,state='ready',pr_url=?,error='',updated_at=? WHERE id=?`, pr, now, a.ID); err != nil {
		return err
	}
	if err = moveWorkflowCard(tx, cardID, current.ProjectID, current.Status, "Done", laneEnd); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM auto_resumes WHERE card_id=?`, cardID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE workflow_interactions SET status='cancelled' WHERE task_id=? AND status IN ('pending','queued')`, a.TaskID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	w.logAttempt(log.WarnLevel, "Card force-readied by operator; readiness checks bypassed", a)
	return nil
}
