package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mark3labs/bonnie/presence"
)

func closedWithin(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

func TestStoreChangesFireOnlyForCommittedRowChanges(t *testing.T) {
	s, _ := workflowFixture(t)
	if _, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('probe','a')`); err != nil {
		t.Fatal(err)
	}
	any, settings, cards := s.Changes(), s.TableChanges("settings"), s.TableChanges("cards")
	// Identical rewrites, reads and rolled-back writes are not changes.
	if _, err := s.db.Exec(`UPDATE settings SET value='a' WHERE key='probe'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE settings SET value='b' WHERE key='missing'`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.writeTx()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`UPDATE settings SET value='rolled-back' WHERE key='probe'`); err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback()
	if _, err = s.AgentRouting(); err != nil {
		t.Fatal(err)
	}
	if closedWithin(any, 50*time.Millisecond) {
		t.Fatal("no-op write notified")
	}
	if _, err = s.db.Exec(`UPDATE settings SET value='b' WHERE key='probe'`); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(any, time.Second) || !closedWithin(settings, time.Second) {
		t.Fatal("committed change did not notify")
	}
	if closedWithin(cards, 50*time.Millisecond) {
		t.Fatal("unrelated table notified")
	}
	if got := s.Changes(); closedWithin(got, 50*time.Millisecond) {
		t.Fatal("feed did not re-arm")
	}
}

func TestPresenceWatchPushesChangesAndExpiry(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	changed := w.PresenceChanges()
	registerChoice(t, w, "watched", true)
	if !closedWithin(changed, 3*time.Second) {
		t.Fatal("registration did not notify")
	}
	changed = w.PresenceChanges()
	if err = w.presence.Unregister(t.Context(), presence.Identity{Agent: "watched", Instance: "test"}); err != nil {
		t.Fatal(err)
	}
	if !closedWithin(changed, 3*time.Second) {
		t.Fatal("unregistration did not notify")
	}
	// TTL expiry emits no KV event; the watcher's expiry timer must notice it.
	js, err := nc.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := js.KeyValue(presenceBucket)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rec := presence.Record{Identity: presence.Identity{Agent: "expiring", Instance: "test"}, State: presence.Ready, UpdatedAt: now, ExpiresAt: now.Add(300 * time.Millisecond)}
	data, _ := json.Marshal(rec)
	next := w.PresenceChanges()
	if _, err = kv.Put("a_expiring", data); err != nil {
		t.Fatal(err)
	}
	// Two notifications follow: the put, then the expiry timer.
	for i, what := range []string{"put", "expiry"} {
		if !closedWithin(next, 3*time.Second) {
			t.Fatalf("%s did not notify (%d)", what, i)
		}
		next = w.PresenceChanges()
	}
	// Idle presence sends nothing.
	if closedWithin(w.PresenceChanges(), 500*time.Millisecond) {
		t.Fatal("idle presence notified")
	}
}

func TestIdleWorkflowDoesNotPoll(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	registerChoice(t, w, "idle-agent", true)
	time.Sleep(300 * time.Millisecond) // Let startup and the registration settle.
	before := w.dispatchPasses.Load()
	time.Sleep(1500 * time.Millisecond)
	if after := w.dispatchPasses.Load(); after != before {
		t.Fatalf("idle workflow ran %d dispatch passes", after-before)
	}
	// A change runs a pass promptly, then the workflow is idle again.
	c := workflowCard(t, s)
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for w.dispatchPasses.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("change did not run dispatch")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	settled := w.dispatchPasses.Load()
	time.Sleep(time.Second)
	if after := w.dispatchPasses.Load(); after != settled {
		t.Fatalf("dispatch did not settle: %d more passes", after-settled)
	}
}

func TestFailingPublishIsPacedNotSpun(t *testing.T) {
	s, nc := workflowFixture(t)
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	c := workflowCard(t, s)
	// Restricted builds with no eligible agent fail to publish every attempt.
	if err = s.SaveAgentRouting(AgentRouting{Agents: map[string]AgentRoles{"nobody": {Plan: true}}}); err != nil {
		t.Fatal(err)
	}
	if err = w.MoveCard(c.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	before := w.dispatchPasses.Load()
	time.Sleep(1500 * time.Millisecond)
	// Backoff of 1s then 2s allows at most a couple of retries here, not a spin.
	if n := w.dispatchPasses.Load() - before; n > 3 {
		t.Fatalf("failing publish spun %d dispatch passes", n)
	}
}

func TestRetrySchedulePacing(t *testing.T) {
	r := newRetrySchedule()
	now := time.Now()
	if !r.due("a", now) {
		t.Fatal("new key not due")
	}
	r.fail("a", now, "fp1")
	if r.due("a", now.Add(500*time.Millisecond), "fp1") || !r.due("a", now.Add(time.Second), "fp1") {
		t.Fatal("first backoff is not 1s")
	}
	if !r.due("a", now, "fp2") {
		t.Fatal("changed fingerprint not due at once")
	}
	r.fail("a", now, "fp1")
	if got := r.next(); !got.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("second backoff %v", got.Sub(now))
	}
	for range 10 {
		r.fail("a", now)
	}
	if got := r.next(); !got.Equal(now.Add(retryMax)) {
		t.Fatalf("backoff not capped: %v", got.Sub(now))
	}
	r.fail("chat:x", now)
	r.expeditePrefix("chat:", now)
	if !r.due("chat:x", now) || r.due("a", now) {
		t.Fatal("prefix expedite")
	}
	r.retain("chat:", map[string]bool{})
	if !r.due("chat:x", now) {
		t.Fatal("retain kept vanished key")
	}
	r.expedite(now)
	if !r.due("a", now) {
		t.Fatal("expedite")
	}
	r.done("a")
	if !r.next().IsZero() {
		t.Fatal("done left a retry")
	}
}
