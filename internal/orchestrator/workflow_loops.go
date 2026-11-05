package orchestrator

import (
	"database/sql"
	"errors"
	"reflect"
	"time"

	"github.com/nats-io/nats.go"
)

// providerCheckInterval paces provider PR/CI/merge checks. Providers offer no
// push channel without webhooks, so this is the one remaining periodic pass.
const providerCheckInterval = 10 * time.Second

// runOnChange runs pass whenever a trigger fires or the deadline pass returned
// arrives (zero means "only on change"). There is no fixed interval: an idle
// workflow sleeps. Triggers are subscribed before each pass, so a change that
// commits during a pass runs it again rather than being missed. A pass writes
// only when it makes progress or records a new failure, and failed items are
// paced by their retry schedule, so self-triggered passes converge.
func (w *Workflow) runOnChange(triggers func() []<-chan struct{}, pass func() time.Time) {
	defer w.wg.Done()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for w.ctx.Err() == nil {
		chans := triggers()
		next := pass()
		timer.Stop()
		var due <-chan time.Time
		if !next.IsZero() {
			timer.Reset(max(time.Until(next), 0))
			due = timer.C
		}
		cases := make([]reflect.SelectCase, 0, len(chans)+2)
		cases = append(cases,
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(w.ctx.Done())},
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(due)})
		for _, ch := range chans {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
		}
		if chosen, _, _ := reflect.Select(cases); chosen == 0 {
			return
		}
	}
}

// watchReconnects returns a feed that fires each time conn reconnects.
func watchReconnects(w *Workflow, conn *nats.Conn) *changeFeed {
	feed := newChangeFeed()
	statuses := conn.StatusChanged(nats.CONNECTED)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer conn.RemoveStatusListener(statuses)
		for {
			select {
			case <-w.ctx.Done():
				return
			case _, ok := <-statuses:
				if !ok {
					return
				}
				feed.notify()
			}
		}
	}()
	return feed
}

// dispatchDeadline is the earliest time dispatch has time-based work: a paced
// retry, a scheduled automatic resume, or a stale-resume check.
func (w *Workflow) dispatchDeadline(now time.Time) time.Time {
	next := w.retry.next()
	if enabled, err := autoResumeEnabled(w.s.db); err == nil && enabled {
		triaged := w.failureClassifier() != nil
		var at sql.NullInt64
		err = w.s.db.QueryRow(`SELECT min(next_check_at) FROM auto_resumes WHERE state='scheduled' AND (?=0 OR triage<>'')`, triaged).Scan(&at)
		if err == nil && at.Valid {
			next = earliest(next, time.UnixMilli(at.Int64))
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			next = earliest(next, now.Add(retryInitial))
		}
	}
	for _, at := range w.resumeChecks {
		next = earliest(next, at)
	}
	// A pending resume becomes eligible for its staleness check at the threshold.
	threshold := w.resumeStaleAfter
	if threshold <= 0 {
		threshold = 5 * time.Minute
	}
	var created sql.NullInt64
	if err := w.s.db.QueryRow(`SELECT min(created_at) FROM failed_resumes WHERE status='pending' AND created_at>?`, now.Add(-threshold).UnixMilli()).Scan(&created); err == nil && created.Valid {
		next = earliest(next, time.UnixMilli(created.Int64).Add(threshold))
	}
	return next
}
