package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/nats-io/nats.go"
	"jawa/internal/runapitoken"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Watch failures invalidate the cached key. Re-establishment is bounded and
// shutdown joins the watcher before the caller closes its shared connection.
func watchAPIKey(ctx context.Context, nc *nats.Conn, agent string) (bonniehttp.Authenticator, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	var key atomic.Pointer[ed25519.PublicKey]
	var checked atomic.Int64
	open := func() (nats.KeyWatcher, error) {
		ready, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		js, err := nc.JetStream(nats.Context(ready))
		if err != nil {
			return nil, err
		}
		kv, err := js.KeyValue(runapitoken.Bucket)
		if err != nil {
			return nil, err
		}
		// The watch owns the lifetime context, not the startup deadline.
		return kv.Watch(runapitoken.PublicKey, nats.Context(ctx))
	}
	watcher, err := open()
	if err != nil {
		cancel()
		return nil, nil, errors.New("jawa: NATS API needs the orchestrator control bucket; upgrade orchestrator or disable --nats-api")
	}
	js, err := nc.JetStream(nats.MaxWait(2 * time.Second))
	if err != nil {
		_ = watcher.Stop()
		cancel()
		return nil, nil, err
	}
	kv, err := js.KeyValue(runapitoken.Bucket)
	if err != nil {
		_ = watcher.Stop()
		cancel()
		return nil, nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// KV watches may silently recover/disconnect without closing Updates. A
		// bounded periodic read expires trust on outage or unnoticed deletion.
		health := time.NewTicker(5 * time.Second)
		defer health.Stop()
		for {
			for {
				select {
				case <-ctx.Done():
					_ = watcher.Stop()
					key.Store(nil)
					return
				case <-health.C:
					entry, err := kv.Get(runapitoken.PublicKey)
					if err != nil || entry.Operation() != nats.KeyValuePut || len(entry.Value()) != ed25519.PublicKeySize {
						key.Store(nil)
						continue
					}
					pub := ed25519.PublicKey(append([]byte(nil), entry.Value()...))
					key.Store(&pub)
					checked.Store(time.Now().UnixNano())
				case entry, ok := <-watcher.Updates():
					if !ok {
						key.Store(nil)
						goto retry
					}
					if entry == nil {
						continue
					}
					if entry.Operation() != nats.KeyValuePut || len(entry.Value()) != ed25519.PublicKeySize {
						key.Store(nil)
						continue
					}
					pub := ed25519.PublicKey(append([]byte(nil), entry.Value()...))
					key.Store(&pub)
					checked.Store(time.Now().UnixNano())
				}
			}
		retry:
			_ = watcher.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Second):
				}
				watcher, err = open()
				if err == nil {
					break
				}
			}
		}
	}()
	auth := func(r *http.Request) (*channel.Principal, error) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		pub := key.Load()
		if !ok || pub == nil || time.Since(time.Unix(0, checked.Load())) > 10*time.Second || runapitoken.Verify(*pub, token, agent, time.Now()) != nil {
			return nil, bonniehttp.ErrUnauthenticated
		}
		return &channel.Principal{Authenticator: "jawa-orchestrator", Kind: "user", ID: "orchestrator"}, nil
	}
	return auth, func() { cancel(); <-done }, nil
}
