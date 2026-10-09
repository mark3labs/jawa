package orchestrator

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

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
	CreatedAt                                           time.Time
}

// NATSConfig deliberately exposes no password or password hash.
type NATSConfig struct{ URL, Username string }
type Store struct{ db *sql.DB }

func OpenStore(path string) (*Store, error) {
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
	tx, err := db.Begin()
	if err != nil {
		return fail(err)
	}
	// Rollback is best-effort; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	fail = func(err error) (*Store, error) { _ = tx.Rollback(); _ = db.Close(); return nil, err }
	_, err = tx.Exec(`
 CREATE TABLE IF NOT EXISTS admin (id INTEGER PRIMARY KEY CHECK(id=1), username TEXT NOT NULL DEFAULT 'admin', password_hash TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL, provider TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '', base_branch TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS cards (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, title TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', status TEXT NOT NULL CHECK(status IN ('Todo','Building','Done')), position INTEGER NOT NULL CHECK(position>=0), created_at INTEGER NOT NULL, issue_url TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)
	if err != nil {
		return fail(err)
	}
	// Inspect the schema rather than suppressing all ALTER TABLE errors.
	for _, m := range []struct{ table, column, definition string }{
		{"admin", "username", "TEXT NOT NULL DEFAULT 'admin'"},
		{"projects", "provider", "TEXT NOT NULL DEFAULT ''"},
		{"projects", "repo", "TEXT NOT NULL DEFAULT ''"},
		{"projects", "base_branch", "TEXT NOT NULL DEFAULT ''"},
		{"cards", "issue_url", "TEXT NOT NULL DEFAULT ''"},
	} {
		var n int
		if err = tx.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, m.table, m.column).Scan(&n); err != nil {
			return fail(err)
		}
		if n == 0 {
			if _, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m.table, m.column, m.definition)); err != nil {
				return fail(err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file:") && path != "" {
		if err = os.Chmod(path, 0600); err != nil {
			return fail(err)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) HasAdmin() (bool, error) {
	var exists bool
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM admin WHERE id=1)`).Scan(&exists)
	return exists, err
}

// First setup wins. A concurrent setup must never replace existing credentials.
func (s *Store) setPassword(hash string) error {
	_, err := s.db.Exec(`INSERT INTO admin(id,username,password_hash) VALUES(1,'admin',?)`, hash)
	return err
}
func (s *Store) checkPassword(hash string) bool {
	var stored string
	return s.db.QueryRow(`SELECT password_hash FROM admin WHERE id=1`).Scan(&stored) == nil && subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) == 1
}

func (s *Store) CreateProject(name string) (Project, error) {
	return s.CreateProjectDetails(name, "", "", "")
}

var scpRepo = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^\s:]+$`)

func validRepository(repo string) bool {
	if strings.ContainsAny(repo, " \t\r\n\x00") {
		return false
	}
	if scpRepo.MatchString(repo) {
		return true
	}
	u, err := url.Parse(repo)
	if err != nil || u.Hostname() == "" || u.Path == "" || u.Path == "/" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || u.Scheme == "ssh" || u.Scheme == "git"
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
		return Project{}, errors.New("invalid repository URL: use git, https or ssh")
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
	tx, err := s.writeTx()
	if err != nil {
		return Card{}, err
	}
	// Rollback is best-effort; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, project).Scan(&exists); err != nil {
		return Card{}, err
	}
	if !exists {
		return Card{}, errors.New("unknown project")
	}
	c := Card{ID: newID(), ProjectID: project, Title: title, Description: desc, IssueURL: issue, Status: "Todo", CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err = tx.QueryRow(`SELECT coalesce(max(position)+1,0) FROM cards WHERE project_id=? AND status='Todo'`, project).Scan(&c.Position); err != nil {
		return Card{}, err
	}
	_, err = tx.Exec(`INSERT INTO cards(id,project_id,title,description,status,position,created_at,issue_url) VALUES(?,?,?,?,?,?,?,?)`, c.ID, c.ProjectID, c.Title, c.Description, c.Status, c.Position, c.CreatedAt.Unix(), c.IssueURL)
	if err != nil {
		return Card{}, err
	}
	if err = tx.Commit(); err != nil {
		return Card{}, err
	}
	return c, nil
}
func (s *Store) Cards(project string) ([]Card, error) {
	q := `SELECT id,project_id,title,description,status,position,created_at,issue_url FROM cards`
	var args []any
	if project != "" {
		q += ` WHERE project_id=?`
		args = append(args, project)
	}
	q += ` ORDER BY CASE status WHEN 'Todo' THEN 0 WHEN 'Building' THEN 1 ELSE 2 END,position,created_at,id`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Card{}
	for rows.Next() {
		var c Card
		var ts int64
		if err := rows.Scan(&c.ID, &c.ProjectID, &c.Title, &c.Description, &c.Status, &c.Position, &ts, &c.IssueURL); err != nil {
			return nil, err
		}
		c.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) MoveCard(id, status string, position int) error {
	if status != "Todo" && status != "Building" && status != "Done" {
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
	// Rebuild both affected lanes in their stable order. Excluding the moving
	// card first makes position mean its final zero-based index in all cases.
	lanes := []string{source}
	if source != status {
		lanes = append(lanes, status)
	}
	for _, lane := range lanes {
		rows, err := tx.Query(`SELECT id FROM cards WHERE project_id=? AND status=? AND id<>? ORDER BY position,created_at,id`, project, lane, id)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var cardID string
			if err = rows.Scan(&cardID); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, cardID)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if lane == status {
			pos := min(position, len(ids))
			ids = append(ids, "")
			copy(ids[pos+1:], ids[pos:])
			ids[pos] = id
		}
		for i, cardID := range ids {
			if _, err = tx.Exec(`UPDATE cards SET status=?,position=? WHERE id=? AND project_id=?`, lane, i, cardID, project); err != nil {
				return err
			}
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
func (s *Store) saveNATS(url, user, pass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
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
