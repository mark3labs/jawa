package orchestrator

import (
	"context"
	"errors"
	"strconv"
	"time"

	"jawa/internal/agentconcurrency"

	"github.com/nats-io/nats.go"
)

// publishAgentConcurrency mirrors only the explicit dashboard override. A missing
// key (including a stale override from a previous process) restores agent defaults.
func publishAgentConcurrency(ctx context.Context, s *Store, nc *nats.Conn) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	values, err := s.DashboardOverrides()
	if err != nil {
		return err
	}
	value := values["agent-concurrency"]
	if value != "" {
		n, err := agentconcurrency.Parse(value)
		if err != nil {
			return err
		}
		value = strconv.Itoa(n)
	}
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return err
	}
	kv, err := js.KeyValue(agentconcurrency.Bucket)
	if err != nil {
		return err
	} // Startup publishes the API key (and creates the bucket) first.
	old, err := kv.Get(agentconcurrency.Key)
	if errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, nats.ErrKeyDeleted) {
		if value == "" {
			return nil
		}
	} else if err != nil {
		return err
	} else if string(old.Value()) == value {
		return nil
	}
	if value == "" {
		return kv.Delete(agentconcurrency.Key)
	}
	_, err = kv.Put(agentconcurrency.Key, []byte(value))
	return err
}

func (w *Workflow) syncAgentConcurrency(ctx context.Context) error {
	// Read persistence while holding the lock, so a delayed sync cannot publish an
	// older snapshot after a newer save/reset has already published.
	w.controlMu.Lock()
	defer w.controlMu.Unlock()
	return publishAgentConcurrency(ctx, w.s, w.resumeConn)
}

func (a *app) syncAgentConcurrency(rctx context.Context, effects *mutationEffects) {
	if a.workflow != nil {
		if err := a.workflow.syncAgentConcurrency(rctx); err != nil {
			effects.flash += " Agent concurrency saved; publication is pending and will be retried."
		}
	}
}
