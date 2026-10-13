package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"jawa/internal/orchestrator/db/sqlc"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

type Project struct {
	ID, Name, Provider, Repo, BaseBranch string
	CreatedAt                            time.Time
}
type Card struct {
	ID, ProjectID, Title, Description, Status, IssueURL string
	Position                                            int
	Archived                                            bool
	CreatedAt                                           time.Time
}

// NATSConfig deliberately exposes no password or password hash.
type NATSConfig struct{ URL, Username string }
type Store struct {
	db *sql.DB
	// runtimeDefaults are command-local and never persisted to dashboard settings.
	runtimeDefaults map[string]string
}

func (s *Store) providerSettings() (map[string]string, error) {
	values, err := s.DashboardOverrides()
	if err != nil {
		return nil, err
	}
	for key, value := range s.runtimeDefaults {
		if _, ok := values[key]; !ok {
			values[key] = value
		}
	}
	return values, nil
}

// passwordHashCost is the bcrypt work factor for stored credentials. Tests
// lower it; production must keep the default.
var passwordHashCost = bcrypt.DefaultCost

// storeTemplate, when set (tests only), holds a fully migrated database image
// that seeds new store files so each one skips re-running every migration.
var storeTemplate []byte

// storeTestPragmas (tests only) runs after the standard PRAGMAs, e.g. to skip
// fsync on throwaway databases.
var storeTestPragmas string

func OpenStore(path string) (*Store, error) {
	if storeTemplate != nil && path != "" && path != ":memory:" && !strings.HasPrefix(path, "file:") {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(path, storeTemplate, 0600); err != nil {
				return nil, err
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// Keep PRAGMAs and in-memory databases on a single persistent connection.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	fail := func(err error) (*Store, error) { _ = db.Close(); return nil, err }
	if _, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		return fail(err)
	}
	if storeTestPragmas != "" {
		if _, err = db.Exec(storeTestPragmas); err != nil {
			return fail(err)
		}
	}
	if err = migrateDatabase(db); err != nil {
		return fail(err)
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") && path != "" {
		if err = os.Chmod(path, 0600); err != nil {
			return fail(err)
		}
	}
	// A previous process cannot still own an in-flight reservation.
	if _, err = db.Exec(`UPDATE issue_submissions SET state='uncertain',updated_at=? WHERE state='reserved'`, time.Now().Unix()); err != nil {
		return fail(err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) HasAdmin() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM admin WHERE id=1)`).Scan(&exists)
	return exists, err
}

// CreateAdmin inserts the singleton account. First setup wins, including concurrent setup.
func (s *Store) CreateAdmin(user, hash string) error {
	_, err := s.db.Exec(`INSERT INTO admin(id,username,password_hash) VALUES(1,?,?)`, user, hash)
	return err
}

func (s *Store) AdminCredentials() (user, hash string, err error) {
	err = s.db.QueryRow(`SELECT username,password_hash FROM admin WHERE id=1`).Scan(&user, &hash)
	return
}

func (s *Store) CreateSession(hash string, exp int64) error {
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash,expires_at) VALUES(?,?)`, hash, exp)
	return err
}

func (s *Store) SessionExpiry(hash string) (exp int64, err error) {
	err = s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash=?`, hash).Scan(&exp)
	return
}

func (s *Store) DeleteSession(hash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hash)
	return err
}

func (s *Store) PruneSessions(now int64) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at<=?`, now)
	return err
}

func (s *Store) SessionOperator(ctx context.Context, hash string, now int64) (user string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT a.username FROM admin a WHERE a.id=1 AND EXISTS(SELECT 1 FROM sessions WHERE token_hash=? AND expires_at>?)`, hash, now).Scan(&user)
	return
}

func (s *Store) CreateProject(name string) (Project, error) {
	return s.CreateProjectDetails(name, "", "", "")
}

var scpRepo = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s:]+$`)

func validRepository(repo string) bool {
	_, _, _, err := providerRepository(repo)
	return err == nil
}

func (s *Store) CreateProjectDetails(name, provider, repo, base string) (Project, error) {
	name, provider, repo, base = strings.TrimSpace(name), strings.TrimSpace(provider), strings.TrimSpace(repo), strings.TrimSpace(base)
	if name == "" {
		return Project{}, errors.New("name required")
	}
	if provider != "" && provider != "github" && provider != "forgejo" {
		return Project{}, errors.New("invalid provider")
	}
	if repo != "" && !validRepository(repo) {
		_, _, _, err := providerRepository(repo)
		return Project{}, fmt.Errorf("invalid repository URL: %w", err)
	}
	p := Project{ID: newID(), Name: name, Provider: provider, Repo: repo, BaseBranch: base, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	_, err := s.db.Exec(`INSERT INTO projects(id,name,created_at,provider,repo,base_branch) VALUES(?,?,?,?,?,?)`, p.ID, p.Name, p.CreatedAt.Unix(), p.Provider, p.Repo, p.BaseBranch)
	if err != nil {
		return Project{}, err
	}
	return p, nil
}
func (s *Store) Projects() ([]Project, error) {
	rows, err := s.db.Query(`SELECT id,name,provider,repo,base_branch,created_at FROM projects ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Project{}
	for rows.Next() {
		var p Project
		var ts int64
		if err := rows.Scan(&p.ID, &p.Name, &p.Provider, &p.Repo, &p.BaseBranch, &ts); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// Acquire the SQLite write lock before reading positions. This also serializes
// writers using other Store instances, not just goroutines using this pool.
func (s *Store) writeTx() (*sql.Tx, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE projects SET id=id WHERE 0`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}
func (s *Store) CreateCard(project, title, desc string) (Card, error) {
	return s.CreateCardDetails(project, title, desc, "")
}
func (s *Store) CreateCardDetails(project, title, desc, issue string) (Card, error) {
	tx, err := s.writeTx()
	if err != nil {
		return Card{}, err
	}
	defer func() { _ = tx.Rollback() }()
	c, err := insertCardDetails(tx, project, title, desc, issue)
	if err != nil {
		return Card{}, err
	}
	if err = tx.Commit(); err != nil {
		return Card{}, err
	}
	return c, nil
}

// Shared by ordinary creation and ledger finalization in the same write transaction.
func insertCardDetails(tx *sql.Tx, project, title, desc, issue string) (Card, error) {
	title = strings.TrimSpace(title)
	issue = strings.TrimSpace(issue)
	if issue != "" {
		u, err := url.Parse(issue)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return Card{}, errors.New("invalid issue URL: use http or https without credentials")
		}
	}
	if title == "" {
		return Card{}, errors.New("title required")
	}
	var exists bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, project).Scan(&exists); err != nil {
		return Card{}, err
	}
	if !exists {
		return Card{}, errors.New("unknown project")
	}
	c := Card{ID: newID(), ProjectID: project, Title: title, Description: desc, IssueURL: issue, Status: "Todo", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	q := sqlc.New(tx)
	ctx := context.Background()
	position, err := q.NextCardPosition(ctx, sqlc.NextCardPositionParams{ProjectID: project, Status: c.Status})
	if err != nil {
		return Card{}, err
	}
	c.Position = int(position)
	err = q.InsertCard(ctx, sqlc.InsertCardParams{
		ID: c.ID, ProjectID: c.ProjectID, Title: c.Title, Description: c.Description,
		Status: c.Status, Position: position, CreatedAt: c.CreatedAt.Unix(), IssueUrl: c.IssueURL,
	})
	return c, err
}

func (s *Store) Cards(project string) ([]Card, error) {
	return s.queryCards(context.Background(), project)
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

// DashboardOverrides returns persisted non-secret orchestrator overrides.
func (s *Store) DashboardOverrides() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key,value FROM settings WHERE key LIKE 'dashboard:%'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		values[strings.TrimPrefix(k, "dashboard:")] = v
	}
	return values, rows.Err()
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
	return tx.Commit()
}
func (s *Store) ClearDashboardOverrides() error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key LIKE 'dashboard:%'`)
	return err
}

func (s *Store) ReplaceDashboardOverrides(values map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec(`DELETE FROM settings WHERE key LIKE 'dashboard:%'`); err != nil {
		return err
	}
	for key, value := range values {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?)`, "dashboard:"+key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) NATSConfig() NATSConfig {
	var c NATSConfig
	// One statement provides a consistent snapshot of both public settings.
	_ = s.db.QueryRow(`SELECT coalesce((SELECT value FROM settings WHERE key='nats_url'),''),coalesce((SELECT value FROM settings WHERE key='nats_username'),'')`).Scan(&c.URL, &c.Username)
	return c
}

// NATSConfigured distinguishes user-confirmed credentials from the random
// bootstrap password. Missing state (including older databases) requires setup.
func (s *Store) NATSConfigured() (bool, error) {
	var ready bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM settings WHERE key='nats_configured' AND value='true')`).Scan(&ready)
	return ready, err
}

func (s *Store) saveNATS(url, user, pass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), passwordHashCost)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	// Rollback is best-effort; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	for _, setting := range []struct{ key, value string }{{"nats_url", url}, {"nats_username", user}, {"nats_password", string(hash)}} {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, setting.key, setting.value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// laneEnd appends a card after all other cards in its destination lane.
const laneEnd = math.MaxInt

// moveWorkflowCard compacts both affected lanes in the caller's transaction.
func moveWorkflowCard(tx *sql.Tx, id, project, source, status string, position int) error {
	q := sqlc.New(tx)
	ctx := context.Background()
	// Excluding the moving card makes position its final zero-based index.
	lanes := []string{source}
	if source != status {
		lanes = append(lanes, status)
	}
	for _, lane := range lanes {
		ids, err := q.ListCardLane(ctx, sqlc.ListCardLaneParams{ProjectID: project, Status: lane, ExcludeID: id})
		if err != nil {
			return err
		}
		if lane == status {
			pos := min(position, len(ids))
			ids = append(ids, "")
			copy(ids[pos+1:], ids[pos:])
			ids[pos] = id
		}
		for i, cardID := range ids {
			if _, err = q.UpdateCardLane(ctx, sqlc.UpdateCardLaneParams{Status: lane, Position: int64(i), ID: cardID, ProjectID: project}); err != nil {
				return err
			}
		}
	}
	return nil
}

// rotateNATSSettings keeps persistence and listener reload in one transaction.
// A failed commit restores the previous listener configuration via rollback.
func (s *Store) rotateNATSSettings(url, user, hash string, reload func() error, rollback func()) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{{"nats_url", url}, {"nats_username", user}, {"nats_password", hash}, {"nats_configured", "true"}} {
		if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1]); err != nil {
			return err
		}
	}
	if err = reload(); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		rollback()
		return err
	}
	return nil
}
