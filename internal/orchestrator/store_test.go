package orchestrator

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func testProject(t *testing.T, s *Store) Project {
	t.Helper()
	p, err := s.CreateProject("project")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func testCard(t *testing.T, s *Store, p Project, title string) Card {
	t.Helper()
	c, err := s.CreateCard(p.ID, title, "description")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func assertLane(t *testing.T, s *Store, p Project, status string, want ...string) {
	t.Helper()
	cards, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, c := range cards {
		if c.Status == status {
			if c.Position != len(got) {
				t.Fatalf("noncompact position: %+v", c)
			}
			got = append(got, c.ID)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%s: got %v want %v", status, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v want %v", status, got, want)
		}
	}
}
func TestStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProjectDetails(" detailed ", "forgejo", "ssh://git@example.org/team/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCardDetails(p.ID, " title ", "description", "https://example.org/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.setPassword("admin-hash"); err != nil {
		t.Fatal(err)
	}
	if err = s.saveNATS("nats://localhost:4222", "agent", "secret"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	projects, err := s.Projects()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projects, []Project{p}) {
		t.Fatalf("projects: %+v", projects)
	}
	cards, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cards, []Card{c}) {
		t.Fatalf("cards: %+v", cards)
	}
	if ok, err := s.HasAdmin(); err != nil || !ok || !s.checkPassword("admin-hash") {
		t.Fatalf("admin: %v %v", ok, err)
	}
	var user, hash string
	if err := s.db.QueryRow(`SELECT username,password_hash FROM admin`).Scan(&user, &hash); err != nil || user != "admin" {
		t.Fatalf("admin username: %q %v", user, err)
	}
	if got := s.NATSConfig(); got != (NATSConfig{URL: "nats://localhost:4222", Username: "agent"}) {
		t.Fatalf("config: %+v", got)
	}
	if err := s.db.QueryRow(`SELECT value FROM settings WHERE key='nats_password'`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == "secret" || bcrypt.CompareHashAndPassword([]byte(hash), []byte("secret")) != nil {
		t.Fatal("NATS password is not bcrypt")
	}
	if err := s.saveNATS("changed", "changed", strings.Repeat("x", 73)); err == nil {
		t.Fatal("expected bcrypt length error")
	}
	if s.NATSConfig().Username != "agent" {
		t.Fatal("failed settings update was not atomic")
	}
}
func TestMoveCardOrdering(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	other := testProject(t, s)
	a, b, c := testCard(t, s, p, "a"), testCard(t, s, p, "b"), testCard(t, s, p, "c")
	untouched := testCard(t, s, other, "other")
	move := func(id, status string, pos int) {
		t.Helper()
		if err := s.MoveCard(id, status, pos); err != nil {
			t.Fatal(err)
		}
	}
	move(c.ID, "Todo", 0)
	assertLane(t, s, p, "Todo", c.ID, a.ID, b.ID)
	move(c.ID, "Todo", 2)
	assertLane(t, s, p, "Todo", a.ID, b.ID, c.ID)
	move(b.ID, "Building", 99)
	assertLane(t, s, p, "Todo", a.ID, c.ID)
	assertLane(t, s, p, "Building", b.ID)
	move(a.ID, "Building", 0)
	assertLane(t, s, p, "Building", a.ID, b.ID)
	assertLane(t, s, p, "Todo", c.ID)
	move(a.ID, "Building", 999)
	assertLane(t, s, p, "Building", b.ID, a.ID)
	move(b.ID, "Done", 0)
	assertLane(t, s, p, "Building", a.ID)
	assertLane(t, s, p, "Done", b.ID)
	move(a.ID, "Todo", 0)
	assertLane(t, s, p, "Todo", a.ID, c.ID)
	assertLane(t, s, p, "Building")
	before, _ := s.Cards("")
	for _, tc := range []struct {
		id, status string
		pos        int
	}{{a.ID, "Invalid", 0}, {a.ID, "Done", -1}, {"missing", "Todo", 0}} {
		if err := s.MoveCard(tc.id, tc.status, tc.pos); err == nil {
			t.Fatalf("accepted invalid move %+v", tc)
		}
	}
	after, _ := s.Cards("")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("invalid moves modified cards")
	}
	assertLane(t, s, other, "Todo", untouched.ID)
}
func TestStoreValidationAndConstraints(t *testing.T) {
	s := testStore(t)
	for _, tc := range []struct{ name, provider, repo string }{{" ", "", ""}, {"p", "gitlab", ""}, {"p", "github", "http://example.org/repo"}, {"p", "forgejo", "file:///tmp/repo"}, {"p", "", "https://"}, {"p", "", "/local/repo"}, {"p", "", "https://example.org/has space"}} {
		if _, err := s.CreateProjectDetails(tc.name, tc.provider, tc.repo, ""); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
	}
	for _, repo := range []string{"https://github.com/org/repo.git", "git://example.org/org/repo", "ssh://git@example.org/org/repo", "git@example.org:org/repo.git"} {
		if _, err := s.CreateProjectDetails("p", "github", repo, "main"); err != nil {
			t.Fatalf("%s: %v", repo, err)
		}
	}
	p := testProject(t, s)
	if _, err := s.CreateCard(p.ID, " \t ", ""); err == nil {
		t.Fatal("blank title accepted")
	}
	if _, err := s.CreateCard("missing", "title", ""); err == nil {
		t.Fatal("missing project accepted")
	}
	c := testCard(t, s, p, "card")
	for _, q := range []string{`UPDATE cards SET status='invalid'`, `UPDATE cards SET project_id='missing'`, `UPDATE cards SET position=-1`} {
		if _, err := s.db.Exec(q); err == nil {
			t.Fatalf("constraint missing: %s", q)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id=?`, p.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM cards WHERE id=?`, c.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: %d %v", n, err)
	}
}
func TestStoreLegacySchemaMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE admin(id INTEGER PRIMARY KEY CHECK(id=1),password_hash TEXT NOT NULL);
 CREATE TABLE projects(id TEXT PRIMARY KEY,name TEXT NOT NULL,created_at INTEGER NOT NULL);
 CREATE TABLE cards(id TEXT PRIMARY KEY,project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,title TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status TEXT NOT NULL CHECK(status IN ('Todo','Building','Done')),position INTEGER NOT NULL,created_at INTEGER NOT NULL);
 INSERT INTO admin(id,password_hash) VALUES(1,'legacy-hash');
 INSERT INTO projects(id,name,created_at) VALUES('p','legacy',1);
 INSERT INTO cards(id,project_id,title,status,position,created_at) VALUES('c','p','legacy','Todo',0,1);`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		var user string
		if err := s.db.QueryRow(`SELECT username FROM admin`).Scan(&user); err != nil || user != "admin" {
			t.Fatalf("migration: %q %v", user, err)
		}
		if !s.checkPassword("legacy-hash") {
			t.Fatal("migration lost password")
		}
		ps, err := s.Projects()
		if err != nil || len(ps) != 1 || ps[0].Provider != "" {
			t.Fatalf("projects: %+v %v", ps, err)
		}
		cs, err := s.Cards("p")
		if err != nil || len(cs) != 1 || cs[0].IssueURL != "" {
			t.Fatalf("cards: %+v %v", cs, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	p := testProject(t, s)
	testCard(t, s, p, "new")
}
func TestConcurrentSetupAndCardCreation(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for range 16 {
		wg.Go(func() { ; results <- s.setPassword("winner") })
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("setup winners: %d", successes)
	}
	if err := s.setPassword("replacement"); err == nil || !s.checkPassword("winner") {
		t.Fatal("setup replaced existing password")
	}
	for range 16 {
		wg.Go(func() {
			if _, err := s.CreateCard(p.ID, "concurrent", ""); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	cards, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 16 {
		t.Fatalf("cards: %d", len(cards))
	}
	for i, c := range cards {
		if c.Position != i {
			t.Fatalf("position: %+v", c)
		}
	}
}

func TestMoveCardRollback(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	a := testCard(t, s, p, "a")
	testCard(t, s, p, "b")
	before, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Source compaction happens first; fail the destination update to verify
	// that those earlier writes are rolled back too.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_move BEFORE UPDATE ON cards WHEN NEW.status='Done' BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.MoveCard(a.ID, "Done", 0); err == nil {
		t.Fatal("expected injected failure")
	}
	after, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("partial move: before=%+v after=%+v", before, after)
	}
}

func TestCardIssueURLValidation(t *testing.T) {
	s := testStore(t)
	p := testProject(t, s)
	for _, issue := range []string{"javascript:alert(1)", "data:text/html,<script>alert(1)</script>", "//example.com/issue", "https:///issue", "https://user:secret@example.com/issue", "https://example.com/\nissue"} {
		t.Run(issue, func(t *testing.T) {
			if _, err := s.CreateCardDetails(p.ID, "unsafe", "", issue); err == nil {
				t.Fatalf("accepted unsafe issue URL %q", issue)
			}
		})
	}
	cards, err := s.Cards(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 0 {
		t.Fatal("invalid URLs persisted cards")
	}
	for _, issue := range []string{"", "https://github.com/owner/repo/issues/1", "http://forgejo.local/owner/repo/issues/2"} {
		card, err := s.CreateCardDetails(p.ID, "safe", "", issue)
		if err != nil {
			t.Fatal(err)
		}
		if card.IssueURL != issue {
			t.Fatalf("issue URL changed: %q", card.IssueURL)
		}
	}
}
