// Package kvfollow keeps a local copy of one JetStream KV key current from the
// bucket's own watch, without periodic reads.
//
// A value is trusted only while it is being followed. Disconnects, a closed
// watch and deletion of the bucket all report Lost at once; a reconnect re-opens
// the watch, which re-delivers the current value. A missing bucket is retried
// when the server announces its creation, with backoff only as a fallback for
// a missed announcement.
package kvfollow

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Config describes the followed key and its callbacks. Callbacks run on the
// follower's goroutine, one at a time.
type Config struct {
	Bucket, Key string
	// Apply receives the current value; present is false when the key or its
	// bucket does not exist (or was deleted or purged).
	Apply func(value []byte, present bool)
	// Lost reports that the last applied value may be stale: the connection
	// dropped or the watch failed. Apply follows once the watch is restored.
	Lost func()
}

const (
	openTimeout = 5 * time.Second
	backoffMin  = time.Second
	backoffMax  = 30 * time.Second
)

// Follow starts following cfg.Key and returns after the first attempt to open
// the watch, with that attempt's error (nil once the initial value has been
// applied). It keeps following, and retrying, until ctx ends or stop is called;
// stop is always non-nil and waits for the follower to exit.
func Follow(ctx context.Context, nc *nats.Conn, cfg Config) (stop func(), err error) {
	ctx, cancel := context.WithCancel(ctx)
	f := &follower{nc: nc, cfg: cfg, ctx: ctx}
	statuses := nc.StatusChanged(nats.CONNECTED, nats.DISCONNECTED, nats.RECONNECTING, nats.CLOSED)
	stream := "KV_" + cfg.Bucket
	advisories := make(chan *nats.Msg, 8)
	var subs []*nats.Subscription
	for _, subject := range []string{
		"$JS.EVENT.ADVISORY.STREAM.CREATED." + stream,
		"$JS.EVENT.ADVISORY.STREAM.DELETED." + stream,
	} {
		sub, e := nc.ChanSubscribe(subject, advisories)
		if e != nil {
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			nc.RemoveStatusListener(statuses)
			cancel()
			return func() {}, e
		}
		subs = append(subs, sub)
	}
	// Advisories must be registered before the first open, or a creation that
	// races it could be missed.
	if e := nc.Flush(); e != nil {
		err = e
	}
	watcher, openErr := f.open()
	if err == nil {
		err = openErr
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		defer func() {
			for _, s := range subs {
				_ = s.Unsubscribe()
			}
			nc.RemoveStatusListener(statuses)
		}()
		f.run(watcher, statuses, advisories)
	})
	return func() { cancel(); wg.Wait() }, err
}

type follower struct {
	nc  *nats.Conn
	cfg Config
	ctx context.Context
}

// open starts a watch and applies the initial value. A missing bucket applies
// "absent" and returns nats.ErrBucketNotFound.
func (f *follower) open() (nats.KeyWatcher, error) {
	ready, stop := context.WithTimeout(f.ctx, openTimeout)
	defer stop()
	js, err := f.nc.JetStream(nats.Context(ready))
	if err != nil {
		f.cfg.Lost()
		return nil, err
	}
	kv, err := js.KeyValue(f.cfg.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		f.cfg.Apply(nil, false)
		return nil, err
	}
	if err != nil {
		f.cfg.Lost()
		return nil, err
	}
	// The watch owns the follower's lifetime, not the open deadline.
	w, err := kv.Watch(f.cfg.Key, nats.Context(f.ctx))
	if err != nil {
		f.cfg.Lost()
		return nil, err
	}
	// Apply the initial value (or its absence) before reporting success. The
	// nil entry marks the end of the initial values, present or not.
	seen := false
	for {
		select {
		case <-ready.Done():
			_ = w.Stop()
			f.cfg.Lost()
			return nil, ready.Err()
		case entry, ok := <-w.Updates():
			if !ok {
				f.cfg.Lost()
				return nil, errors.New("kvfollow: watch closed")
			}
			if entry == nil {
				if !seen {
					f.cfg.Apply(nil, false)
				}
				return w, nil
			}
			seen = true
			f.apply(entry)
		}
	}
}

func (f *follower) apply(entry nats.KeyValueEntry) {
	if entry.Operation() == nats.KeyValuePut {
		f.cfg.Apply(entry.Value(), true)
	} else {
		f.cfg.Apply(nil, false)
	}
}

func (f *follower) run(w nats.KeyWatcher, statuses chan nats.Status, advisories chan *nats.Msg) {
	backoff := backoffMin
	retry := time.NewTimer(time.Hour)
	retry.Stop()
	defer retry.Stop()
	if w == nil {
		retry.Reset(backoff)
	}
	// Stopping a watch may wait on a JetStream API timeout while disconnected,
	// so old watches stop in the background and never delay a Lost report.
	var stopping sync.WaitGroup
	defer stopping.Wait()
	stopWatch := func() {
		if w != nil {
			old := w
			w = nil
			stopping.Go(func() { _ = old.Stop() })
		}
	}
	defer stopWatch()
	reopen := func() {
		stopWatch()
		retry.Stop()
		var err error
		if w, err = f.open(); err != nil {
			retry.Reset(backoff)
			backoff = min(backoff*2, backoffMax)
			return
		}
		backoff = backoffMin
	}
	var updates <-chan nats.KeyValueEntry
	for {
		updates = nil
		if w != nil {
			updates = w.Updates()
		}
		select {
		case <-f.ctx.Done():
			return
		case entry, ok := <-updates:
			if !ok {
				f.cfg.Lost()
				w = nil
				reopen()
				continue
			}
			if entry != nil {
				f.apply(entry)
			}
		case status := <-statuses:
			if status == nats.CONNECTED {
				// A reconnect may have missed updates: re-read from a fresh watch.
				reopen()
			} else {
				f.cfg.Lost()
				stopWatch()
				retry.Stop()
			}
		case msg := <-advisories:
			if msg.Subject == "$JS.EVENT.ADVISORY.STREAM.DELETED.KV_"+f.cfg.Bucket {
				f.cfg.Apply(nil, false)
				stopWatch()
				retry.Stop()
				// Wait for the creation advisory; backoff covers a missed one.
				backoff = backoffMin
				retry.Reset(backoff)
				continue
			}
			reopen()
		case <-retry.C:
			reopen()
		}
	}
}
