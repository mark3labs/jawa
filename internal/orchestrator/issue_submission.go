package orchestrator

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type issueSubmission struct {
	Token, Project, Provider, Repository, Title, Body, State, URL, Card string
	Number                                                              int
}

func readIssueSubmission(tx *sql.Tx, token string) (s issueSubmission, err error) {
	err = tx.QueryRow(`SELECT token,project_id,provider,repository,title,body,state,issue_url,issue_number,card_id FROM issue_submissions WHERE token=?`, token).Scan(&s.Token, &s.Project, &s.Provider, &s.Repository, &s.Title, &s.Body, &s.State, &s.URL, &s.Number, &s.Card)
	return
}
func validSubmissionToken(token string) bool {
	b, e := hex.DecodeString(token)
	return e == nil && len(b) == 32
}

// Reserve before dispatch; only the inserting caller is allowed to send a POST.
func (s *Store) reserveIssueSubmission(token string, p Project, title, body string) (issueSubmission, bool, error) {
	if !validSubmissionToken(token) {
		return issueSubmission{}, false, errors.New("Invalid submission token. Reopen New card.")
	}
	tx, err := s.writeTx()
	if err != nil {
		return issueSubmission{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := readIssueSubmission(tx, token)
	fresh := errors.Is(err, sql.ErrNoRows)
	if fresh {
		now := time.Now().Unix()
		_, err = tx.Exec(`INSERT INTO issue_submissions(token,project_id,provider,repository,title,body,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'reserved',?,?)`, token, p.ID, p.Provider, p.Repo, title, body, now, now)
		row = issueSubmission{Token: token, Project: p.ID, Provider: p.Provider, Repository: p.Repo, Title: title, Body: body, State: "reserved"}
	} else if err == nil && (row.Project != p.ID || row.Provider != p.Provider || row.Repository != p.Repo || row.Title != title || row.Body != body) {
		return row, false, errors.New("This submission token belongs to a different draft. Restore the original draft to recover it; no issue was retried.")
	}
	if err != nil {
		return row, false, err
	}
	err = tx.Commit()
	return row, fresh, err
}
func (s *Store) recordIssueOutcome(token, state string, issue projectIssue) error {
	_, err := s.db.Exec(`UPDATE issue_submissions SET state=?,issue_url=?,issue_number=?,updated_at=? WHERE token=? AND state='reserved'`, state, issue.URL, issue.Number, time.Now().Unix(), token)
	return err
}

// Serialize card insertion and completion. Completed tombstones survive card deletion.
func (s *Store) finalizeIssueSubmission(token, recoveryURL string, recovery bool) error {
	tx, err := s.writeTx()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	row, err := readIssueSubmission(tx, token)
	if err != nil {
		return err
	}
	if row.State == "completed" {
		return nil
	}
	// A reserved write may still be in flight: recovery cannot race its outcome.
	if row.State == "reserved" {
		return errors.New("Issue submission is reserved or interrupted. Check repository before recovery.")
	}
	issue := row.URL
	if row.State != "created" {
		if !recovery {
			return errors.New("Explicit linking or unlinked recovery is required.")
		}
		issue = recoveryURL
	}
	card, err := insertCardDetails(tx, row.Project, row.Title, row.Body, issue)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE issue_submissions SET state='completed',card_id=?,updated_at=? WHERE token=?`, card.ID, time.Now().Unix(), token)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func canonicalRecoveryIssue(p Project, raw string) (string, error) {
	host, owner, repo, err := providerRepository(p.Repo)
	if err != nil {
		return "", errors.New("Unsupported project repository for issue recovery.")
	}
	prefix := "https://" + host + "/" + owner + "/" + repo + "/issues/"
	raw = strings.TrimSpace(raw)
	number, err := strconv.Atoi(strings.TrimPrefix(raw, prefix))
	if !strings.HasPrefix(raw, prefix) || err != nil || number < 1 || raw != prefix+strconv.Itoa(number) {
		return "", errors.New("Link a canonical issue URL from this project's repository.")
	}
	return raw, nil
}

func (a *app) submitIssueCard(r *http.Request) error {
	title, body := strings.TrimSpace(r.FormValue("title")), r.FormValue("description")
	if title == "" {
		return errors.New("Title is required.")
	}
	projects, err := a.s.Projects()
	if err != nil {
		return errors.New("Unable to load projects.")
	}
	var p Project
	for _, candidate := range projects {
		if candidate.ID == r.FormValue("project_id") {
			p = candidate
			break
		}
	}
	if p.ID == "" {
		return errors.New("Unknown project.")
	}
	// Explicit rejection retry changes identity only after checking the previous definite rejection.
	if r.FormValue("issue_recovery") == "retry" {
		tx, e := a.s.writeTx()
		if e != nil {
			return errors.New("Unable to load rejected submission.")
		}
		previous, e := readIssueSubmission(tx, r.FormValue("issue_submission"))
		_ = tx.Rollback()
		if e != nil || previous.State != "rejected" || previous.Project != p.ID || previous.Provider != p.Provider || previous.Repository != p.Repo {
			return errors.New("Only a definitely rejected submission can be explicitly retried.")
		}
		r.Form.Set("issue_submission", tokenHash(previous.Token+":explicit-rejected-retry"))
		r.Form.Set("issue_recovery", "")
		r.Form.Set("create_issue", "yes")
	}
	create := r.FormValue("create_issue") != ""
	intent := r.FormValue("issue_recovery")
	linked := strings.TrimSpace(r.FormValue("issue_url"))
	if create && (linked != "" || intent != "") {
		return errors.New("Choose either issue creation or linking/recovery, not both.")
	}
	recoveryURL := strings.TrimSpace(r.FormValue("recovery_url"))
	if !create {
		switch intent {
		case "link":
			if recoveryURL == "" {
				recoveryURL = linked
			}
			recoveryURL, err = canonicalRecoveryIssue(p, recoveryURL)
			if err != nil {
				return err
			}
		case "unlinked":
			if linked != "" || recoveryURL != "" {
				return errors.New("Unlinked recovery cannot include an issue URL.")
			}
		default:
			if !a.hasIssueSubmission(r.FormValue("issue_submission")) {
				return errors.New("Choose explicit issue recovery.")
			}
		}
	}
	settings, err := a.s.providerSettings()
	if err != nil {
		return errors.New("Unable to load provider settings.")
	}
	// Validate targets/credentials before a new reservation. Existing outcomes remain recoverable without credentials.
	token := r.FormValue("issue_submission")
	var count int
	if err = a.s.db.QueryRow(`SELECT count(*) FROM issue_submissions WHERE token=?`, token).Scan(&count); err != nil {
		return errors.New("Unable to load submission.")
	}
	if !create && count == 0 {
		return errors.New("No previous issue submission to recover.")
	}
	if create && count == 0 {
		if _, _, _, err = issueCreateTarget(p, settings); err != nil {
			return err
		}
	}
	row, fresh, err := a.s.reserveIssueSubmission(token, p, title, body)
	if err != nil {
		return errors.New("Unable to reserve submission, or its token belongs to a different draft. Restore the original draft to recover; no issue was retried.")
	}
	if !create && fresh {
		return errors.New("No previous issue submission to recover.")
	}
	if row.State == "completed" {
		return nil
	}
	if row.State == "created" {
		if err = a.s.finalizeIssueSubmission(token, "", false); err != nil {
			return errors.New("Issue was created, but the card could not be saved. Submit again to retry only local card saving.")
		}
		return nil
	}
	if !fresh && row.State == "reserved" {
		// Never reinterpret a live reservation as retryable. On startup reservations become uncertain.
		return errors.New("Issue submission is busy or interrupted. Do not retry creation. Check repository; after restart explicit recovery is available.")
	}
	if !create {
		if intent != "link" && intent != "unlinked" {
			return errors.New("Choose explicit linking or unlinked recovery.")
		}
		if err = a.s.finalizeIssueSubmission(token, recoveryURL, true); err != nil {
			return errors.New("The recovery card could not be saved. Keep this draft and retry local recovery.")
		}
		return nil
	}
	if !fresh {
		return errors.New("Issue creation was already attempted. Refresh issues / check repository and recover, or explicitly retry a rejected request with a new submission token.")
	}
	issue, createErr := createProjectIssue(r.Context(), p, title, body, a.issueClient, settings)
	state := "created"
	if createErr != nil {
		state = "uncertain"
		var ce *issueCreateError
		if errors.As(createErr, &ce) && !ce.Uncertain {
			state = "rejected"
		}
	}
	if err = a.s.recordIssueOutcome(token, state, issue); err != nil {
		return errors.New("Unable to record issue result. Do not retry creation. Check repository and restart for recovery.")
	}
	if createErr != nil {
		return createErr
	}
	if err = a.s.finalizeIssueSubmission(token, "", false); err != nil {
		return errors.New("Issue was created, but the card could not be saved. Submit again to retry only local card saving.")
	}
	return nil
}

func (a *app) hasIssueSubmission(token string) bool {
	var n int
	return a.s.db.QueryRow(`SELECT count(*) FROM issue_submissions WHERE token=?`, token).Scan(&n) == nil && n > 0
}
