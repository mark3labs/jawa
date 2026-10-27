// Package agentconcurrency defines the shared agent execution policy.
package agentconcurrency

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

const (
	Bucket  = "jawa_control"
	Key     = "agent_concurrency"
	Default = 4
	Max     = 1024
)

func Parse(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > Max {
		return 0, errors.New("agent concurrency must be an integer between 1 and 1024")
	}
	return n, nil
}

// Gate bounds live model executions, including continuations and recovery.
// Resizing never interrupts an existing holder.
type Gate struct {
	mu            sync.Mutex
	limit, active int
	changed       chan struct{}
}

func New(limit int) *Gate { return &Gate{limit: limit, changed: make(chan struct{})} }
func (g *Gate) Set(limit int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if limit < 1 || limit > Max || limit == g.limit {
		return
	}
	g.limit = limit
	close(g.changed)
	g.changed = make(chan struct{})
}
func (g *Gate) Acquire(ctx context.Context) error {
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return err
		}
		if g.active < g.limit {
			g.active++
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}
func (g *Gate) Release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.active--
	close(g.changed)
	g.changed = make(chan struct{})
}
