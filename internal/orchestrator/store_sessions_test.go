package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestStoreSessions(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if _, _, err := s.AdminCredentials(); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing admin: %v", err)
	}
	if err := s.CreateAdmin("operator", "hash"); err != nil {
		t.Fatal(err)
	}
	for _, session := range []struct {
		hash   string
		expiry int64
	}{{"live", 101}, {"expired", 99}, {"boundary", 100}} {
		if err := s.CreateSession(session.hash, session.expiry); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession("live", 200); err == nil {
		t.Fatal("duplicate session replaced expiry")
	}
	if exp, err := s.SessionExpiry("live"); err != nil || exp != 101 {
		t.Fatalf("expiry: %d %v", exp, err)
	}
	if user, err := s.SessionOperator(context.Background(), "live", 100); err != nil || user != "operator" {
		t.Fatalf("operator: %q %v", user, err)
	}
	for _, hash := range []string{"expired", "boundary", "missing"} {
		if _, err := s.SessionOperator(context.Background(), hash, 100); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("invalid session %s: %v", hash, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.SessionOperator(ctx, "live", 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lookup: %v", err)
	}
	if err := s.PruneSessions(100); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{"expired", "boundary"} {
		if _, err := s.SessionExpiry(hash); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("pruned session %s: %v", hash, err)
		}
	}
	if exp, err := s.SessionExpiry("live"); err != nil || exp != 101 {
		t.Fatalf("live session pruned: %d %v", exp, err)
	}
	if err := s.DeleteSession("live"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession("live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionOperator(context.Background(), "live", 100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted session: %v", err)
	}
}

func TestRotateNATSSettingsReloadRollback(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := s.saveNATS("nats://old:4222", "old", "old-password"); err != nil {
		t.Fatal(err)
	}
	reloadErr := errors.New("reload failed")
	rollbackCalled := false
	err := s.rotateNATSSettings("nats://new:4222", "new", "hash", func() error { return reloadErr }, func() { rollbackCalled = true })
	if !errors.Is(err, reloadErr) || rollbackCalled {
		t.Fatalf("reload failure: %v rollback=%v", err, rollbackCalled)
	}
	if cfg := s.NATSConfig(); cfg.URL != "nats://old:4222" || cfg.Username != "old" {
		t.Fatalf("partial settings: %+v", cfg)
	}
	if ready, err := s.NATSConfigured(); err != nil || ready {
		t.Fatalf("configured after rollback: %v %v", ready, err)
	}
}

func TestRotateNATSSettingsCommitRollback(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := s.saveNATS("nats://old:4222", "old", "old-password"); err != nil {
		t.Fatal(err)
	}
	// Defer a foreign-key failure until commit, after the listener has reloaded.
	if _, err := s.db.Exec(`CREATE TABLE rotation_parent(id INTEGER PRIMARY KEY);
 CREATE TABLE rotation_child(id INTEGER REFERENCES rotation_parent(id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER fail_rotation AFTER INSERT ON settings WHEN NEW.key='nats_configured'
 BEGIN INSERT INTO rotation_child VALUES(1); END;`); err != nil {
		t.Fatal(err)
	}
	reloaded, restored := false, false
	err := s.rotateNATSSettings("nats://new:4222", "new", "hash", func() error { reloaded = true; return nil }, func() { restored = true })
	if err == nil || !reloaded || !restored {
		t.Fatalf("commit failure: %v reload=%v restore=%v", err, reloaded, restored)
	}
	if cfg := s.NATSConfig(); cfg.URL != "nats://old:4222" || cfg.Username != "old" {
		t.Fatalf("partial settings: %+v", cfg)
	}
	if ready, err := s.NATSConfigured(); err != nil || ready {
		t.Fatalf("configured after rollback: %v %v", ready, err)
	}
}
