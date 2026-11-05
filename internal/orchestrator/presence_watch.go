package orchestrator

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/mark3labs/bonnie/presence"
	"github.com/nats-io/nats.go"
)

const presenceBucket = "jawa_agents"

// PresenceChanges closes after the next agent presence change, including
// heartbeat refreshes.
func (w *Workflow) PresenceChanges() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.presenceChanges.C()
}

// presenceMaterial closes when an agent appears, leaves, expires or changes
// state, endpoints or labels; heartbeat-only refreshes do not fire it. Background
// work uses it so heartbeats do not become a polling interval.
func (w *Workflow) presenceMaterial() <-chan struct{} {
	if w == nil {
		return nil
	}
	return w.presenceMaterialChanges.C()
}

// presenceFingerprint is a record without its refresh timestamps.
func presenceFingerprint(r presence.Record) string {
	r.UpdatedAt, r.ExpiresAt = time.Time{}, time.Time{}
	data, _ := json.Marshal(r)
	return string(data)
}

// watchPresence pushes presence changes from the KV bucket's own watch, rather
// than re-listing the bucket on a timer. TTL expiry produces no KV event, so a
// single timer fires at the earliest known ExpiresAt instead.
func (w *Workflow) watchPresence(conn *nats.Conn) {
	defer w.wg.Done()
	backoff := time.Second
	for w.ctx.Err() == nil {
		started := time.Now()
		err := w.watchPresenceOnce(conn)
		if w.ctx.Err() != nil {
			return
		}
		// The watch is gone; viewers must not trust what they last rendered.
		w.presenceChanges.notify()
		w.presenceMaterialChanges.notify()
		if err != nil {
			w.log().Warn("Presence watch failed; retrying", "category", consumerErrorCategory(err))
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (w *Workflow) watchPresenceOnce(conn *nats.Conn) error {
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	kv, err := js.KeyValue(presenceBucket)
	if err != nil {
		return err
	}
	watcher, err := kv.WatchAll(nats.Context(w.ctx))
	if err != nil {
		return err
	}
	defer func() { _ = watcher.Stop() }()
	expires := map[string]time.Time{}
	seen := map[string]string{} // key -> presenceFingerprint
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	arm := func() {
		timer.Stop()
		var earliest time.Time
		for _, at := range expires {
			if earliest.IsZero() || at.Before(earliest) {
				earliest = at
			}
		}
		if !earliest.IsZero() {
			timer.Reset(max(time.Until(earliest), 0))
		}
	}
	for {
		select {
		case <-w.ctx.Done():
			return nil
		case entry, ok := <-watcher.Updates():
			if !ok {
				return errors.New("presence watch closed")
			}
			if entry == nil { // End of the initial values.
				continue
			}
			key := entry.Key()
			if !strings.HasPrefix(key, "a_") {
				continue
			}
			var r presence.Record
			material := false
			if entry.Operation() == nats.KeyValuePut && json.Unmarshal(entry.Value(), &r) == nil && !r.ExpiresAt.IsZero() {
				expires[key] = r.ExpiresAt
				if fp := presenceFingerprint(r); seen[key] != fp {
					seen[key], material = fp, true
				}
			} else {
				delete(expires, key)
				_, material = seen[key]
				delete(seen, key)
			}
			arm()
			w.presenceChanges.notify()
			if material {
				w.presenceMaterialChanges.notify()
			}
		case <-timer.C:
			now := time.Now()
			for key, at := range expires {
				if !at.After(now) {
					delete(expires, key)
					delete(seen, key)
				}
			}
			arm()
			w.presenceChanges.notify()
			w.presenceMaterialChanges.notify()
		}
	}
}
