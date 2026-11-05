package reportbridge

import (
	"context"

	"jawa/internal/agentconcurrency"
	"jawa/internal/kvfollow"

	"github.com/nats-io/nats.go"
)

// SetConcurrency configures the process-wide execution gate before serving.
func (b *Bridge) SetConcurrency(limit int) {
	b.gate = agentconcurrency.New(limit)
	b.concurrencyDefault = limit
}

// The policy follows the control bucket's watch rather than periodic reads. A
// missing key or bucket restores the default; a lost connection retains the
// last known policy until the reconnected watch re-reads it.
func (c *reportChannel) startConcurrency(ctx context.Context, conn *nats.Conn) {
	if c.bridge.gate == nil {
		return
	}
	stop, _ := kvfollow.Follow(ctx, conn, kvfollow.Config{
		Bucket: agentconcurrency.Bucket,
		Key:    agentconcurrency.Key,
		Apply: func(value []byte, present bool) {
			if !present {
				c.bridge.gate.Set(c.bridge.concurrencyDefault)
				return
			}
			if n, err := agentconcurrency.Parse(string(value)); err == nil {
				c.bridge.gate.Set(n)
			}
		},
		Lost: func() {},
	})
	c.concurrencyWG.Go(func() {
		<-ctx.Done()
		stop()
	})
}
