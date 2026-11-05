package orchestrator

import (
	"strings"
	"sync"
	"time"
)

// Retry pacing for work whose last remote attempt failed or is still waiting
// on the remote side. Workflow loops run on state changes; a failed item is
// skipped until its backoff elapses, so its own error write cannot spin the
// loop, and the loop sleeps until the earliest due retry.
const (
	retryInitial = time.Second
	retryMax     = 30 * time.Second
)

type retryItem struct {
	at          time.Time
	delay       time.Duration
	fingerprint string
}

type retrySchedule struct {
	mu    sync.Mutex
	items map[string]retryItem
}

func newRetrySchedule() *retrySchedule { return &retrySchedule{items: map[string]retryItem{}} }

// due reports whether key may be attempted now. A changed fingerprint (for
// example the run's event sequence) makes it due at once: the remote side
// moved, so waiting out the backoff would only add latency.
func (r *retrySchedule) due(key string, now time.Time, fingerprint ...string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	it, ok := r.items[key]
	if !ok {
		return true
	}
	if len(fingerprint) > 0 && fingerprint[0] != it.fingerprint {
		return true
	}
	return !now.Before(it.at)
}

// fail schedules the next attempt with exponential backoff.
func (r *retrySchedule) fail(key string, now time.Time, fingerprint ...string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	it := r.items[key]
	if it.delay == 0 {
		it.delay = retryInitial
	} else {
		it.delay = min(it.delay*2, retryMax)
	}
	it.at = now.Add(it.delay)
	if len(fingerprint) > 0 {
		it.fingerprint = fingerprint[0]
	}
	r.items[key] = it
}

func (r *retrySchedule) done(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.items, key)
	r.mu.Unlock()
}

// expedite makes every paced retry due now while keeping its backoff level:
// an agent appeared or NATS reconnected, so remote work is likely to succeed.
func (r *retrySchedule) expedite(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, it := range r.items {
		it.at = now
		r.items[key] = it
	}
}

// expeditePrefix makes paced retries under prefix due now.
func (r *retrySchedule) expeditePrefix(prefix string, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, it := range r.items {
		if strings.HasPrefix(key, prefix) {
			it.at = now
			r.items[key] = it
		}
	}
}

// retain drops keys under prefix whose work no longer exists, so a vanished
// item never keeps scheduling wakeups.
func (r *retrySchedule) retain(prefix string, keep map[string]bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.items {
		if strings.HasPrefix(key, prefix) && !keep[key] {
			delete(r.items, key)
		}
	}
}

// next returns the earliest scheduled retry, or zero when none is pending.
func (r *retrySchedule) next() time.Time {
	if r == nil {
		return time.Time{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var earliest time.Time
	for _, it := range r.items {
		if earliest.IsZero() || it.at.Before(earliest) {
			earliest = it.at
		}
	}
	return earliest
}

// earliest returns the earlier non-zero time.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
