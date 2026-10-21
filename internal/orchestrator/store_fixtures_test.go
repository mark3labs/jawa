package orchestrator

import (
	"context"
	"database/sql"
	"errors"
)

// Legacy Store mutators are available only to tests and fixtures.
func (s *Store) CreateProject(name string) (Project, error) {
	return s.CreateProjectDetails(name, "", "", "")
}

func (s *Store) CreateCard(project, title, desc string) (Card, error) {
	return s.CreateCardDetails(project, title, desc, "")
}

func (s *Store) CreateCardDetails(project, title, desc, issue string) (Card, error) {
	return s.CreateCardOptions(project, title, desc, issue, "inherit")
}

// SetCardArchived changes only the independent archive flag; card history and workflow are untouched.
func (s *Store) SetCardArchived(id string, archived bool) error {
	n, err := s.setCardArchived(context.Background(), id, archived)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("card not found")
	}
	return nil
}

// MoveCard is a non-workflow primitive kept for tests and fixtures. Production
// mutations must use Workflow.MoveCard to enforce execution rules.
func (s *Store) MoveCard(id, status string, position int) error {
	if status != "Todo" && status != "Planning" && status != "Building" && status != "Fix" && status != "Done" {
		return errors.New("invalid status")
	}
	if position < 0 {
		return errors.New("negative position")
	}
	tx, err := s.writeTx()
	if err != nil {
		return err
	}
	// Rollback is best-effort; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	var project, source string
	if err = tx.QueryRow(`SELECT project_id,status FROM cards WHERE id=?`, id).Scan(&project, &source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("card not found")
		}
		return err
	}
	// Planning must never be entered/exited without the workflow transaction.
	if (status == "Planning" || source == "Planning") && source != status {
		return errors.New("planning transitions require workflow")
	}
	if (status == "Fix" || source == "Fix") && source != status {
		return errors.New("fix transitions require workflow")
	}
	if err = moveWorkflowCard(tx, id, project, source, status, position); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SetDashboardOverrides(values map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for k, v := range values {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "dashboard:"+k, v); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES('auto-merge:generation','1') ON CONFLICT(key) DO UPDATE SET value=CAST(CAST(value AS INTEGER)+1 AS TEXT)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE auto_merge_operations SET state='deferred',category='policy_changed',next_check_at=0 WHERE state IN ('reserved','deferred','rejected')`); err != nil {
		return err
	}
	return tx.Commit()
}

// Reserve before dispatch; only the inserting caller is allowed to send a POST.
func (s *Store) reserveIssueSubmission(token string, p Project, title, body string) (issueSubmission, bool, error) {
	return s.reserveIssueSubmissionOptions(token, p, title, body, "inherit")
}
