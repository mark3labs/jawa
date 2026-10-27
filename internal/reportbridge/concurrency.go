package reportbridge

import (
	"context"
	"errors"
	"time"

	"jawa/internal/agentconcurrency"

	"github.com/nats-io/nats.go"
)

// SetConcurrency configures the process-wide execution gate before serving.
func (b *Bridge) SetConcurrency(limit int) {
	b.gate = agentconcurrency.New(limit)
	b.concurrencyDefault = limit
}

// Polling uses fresh KV reads, so reconnects and bucket recreation cannot leave
// an orphaned watch. A transport failure retains the last known policy.
func (c *reportChannel) startConcurrency(ctx context.Context, conn *nats.Conn) {
	if c.bridge.gate == nil {
		return
	}
	syncPolicy := func() {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		js, err := conn.JetStream(nats.Context(readCtx))
		if err != nil {
			return
		}
		kv, err := js.KeyValue(agentconcurrency.Bucket)
		if errors.Is(err, nats.ErrBucketNotFound) {
			c.bridge.gate.Set(c.bridge.concurrencyDefault)
			return
		}
		if err != nil {
			return
		}
		entry, err := kv.Get(agentconcurrency.Key)
		if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, nats.ErrKeyDeleted) {
			c.bridge.gate.Set(c.bridge.concurrencyDefault)
		} else if err == nil {
			if n, err := agentconcurrency.Parse(string(entry.Value())); err == nil {
				c.bridge.gate.Set(n)
			}
		}
	}
	syncPolicy()
	c.concurrencyWG.Go(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				syncPolicy()
			}
		}
	})
}
