package orchestrator

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"sync"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// changeFeed broadcasts "something changed" without carrying data. Waiters take
// C before reading state and re-read after it closes, so no change is missed.
// Nothing is sent while nothing changes: there is no timer behind it.
type changeFeed struct {
	mu sync.Mutex
	ch chan struct{}
}

func newChangeFeed() *changeFeed { return &changeFeed{ch: make(chan struct{})} }

// C returns a channel that closes at the next change.
func (f *changeFeed) C() <-chan struct{} {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ch
}

func (f *changeFeed) notify() {
	if f == nil {
		return
	}
	f.mu.Lock()
	close(f.ch)
	f.ch = make(chan struct{})
	f.mu.Unlock()
}

// storeChanges records which tables a committed transaction actually changed.
// Updates that rewrite identical values are not changes, so idempotent writes
// (for example re-recording the same retry error) wake nobody.
type storeChanges struct {
	any    *changeFeed
	mu     sync.Mutex
	tables map[string]*changeFeed
}

func newStoreChanges() *storeChanges {
	return &storeChanges{any: newChangeFeed(), tables: map[string]*changeFeed{}}
}

func (c *storeChanges) table(name string) *changeFeed {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.tables[name]
	if !ok {
		f = newChangeFeed()
		c.tables[name] = f
	}
	return f
}

func (c *storeChanges) committed(tables map[string]bool) {
	c.mu.Lock()
	feeds := make([]*changeFeed, 0, len(tables))
	for name := range tables {
		if f, ok := c.tables[name]; ok {
			feeds = append(feeds, f)
		}
	}
	c.mu.Unlock()
	for _, f := range feeds {
		f.notify()
	}
	c.any.notify()
}

// Changes closes after the next committed change to any table.
// A Store built without OpenStore has no feed; its channels never close.
func (s *Store) Changes() <-chan struct{} {
	if s.changes == nil {
		return nil
	}
	return s.changes.any.C()
}

// TableChanges closes after the next committed change to table.
func (s *Store) TableChanges(table string) <-chan struct{} {
	if s.changes == nil {
		return nil
	}
	return s.changes.table(table).C()
}

type hookedConn interface {
	RegisterPreUpdateHook(sqlite.PreUpdateHookFn)
	RegisterCommitHook(sqlite.CommitHookFn)
	RegisterRollbackHook(sqlite.RollbackHookFn)
}

// changeConnector installs change hooks on every connection it opens.
//
// The commit hook runs just before SQLite finalizes the commit. The Store keeps
// a single connection, so a woken reader cannot query until that commit has
// finished and always sees the committed rows.
type changeConnector struct {
	driver.Connector
	changes *storeChanges
}

func (c changeConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	hooked, ok := conn.(hookedConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("sqlite connection does not support change hooks")
	}
	var mu sync.Mutex
	pending := map[string]bool{}
	hooked.RegisterPreUpdateHook(func(d sqlite.SQLitePreUpdateData) {
		if d.Op == sqlite3.SQLITE_UPDATE && sameRow(&d) {
			return
		}
		mu.Lock()
		pending[d.TableName] = true
		mu.Unlock()
	})
	hooked.RegisterCommitHook(func() int32 {
		mu.Lock()
		tables := pending
		pending = map[string]bool{}
		mu.Unlock()
		if len(tables) > 0 {
			c.changes.committed(tables)
		}
		return 0 // Never veto the commit.
	})
	hooked.RegisterRollbackHook(func() {
		mu.Lock()
		pending = map[string]bool{}
		mu.Unlock()
	})
	return conn, nil
}

// sameRow reports whether an UPDATE leaves every column unchanged. Read
// failures count as a change.
func sameRow(d *sqlite.SQLitePreUpdateData) bool {
	n := d.Count()
	if n <= 0 {
		return false
	}
	// The driver assigns each value into the dest slot itself (convertAssign
	// on &dest[i]), so pass plain slots, not pointers.
	oldRow, newRow := make([]any, n), make([]any, n)
	if d.Old(oldRow...) != nil || d.New(newRow...) != nil {
		return false
	}
	return d.OldRowID == d.NewRowID && reflect.DeepEqual(oldRow, newRow)
}
