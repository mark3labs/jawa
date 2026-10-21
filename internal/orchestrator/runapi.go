package orchestrator

import (
	"context"
	"crypto/ed25519"
	"errors"
	bonnieclient "github.com/mark3labs/bonnie/client"
	"github.com/mark3labs/bonnie/client/natsapi"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"jawa/internal/runapitoken"
	"net/http"
	"sync"
	"time"
)

var errRunBusy = errors.New("run observation capacity reached")

type apiBearer struct {
	next  http.RoundTripper
	key   ed25519.PrivateKey
	agent string
}

func (b *apiBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header = r.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+runapitoken.Sign(b.key, b.agent, time.Now()))
	return b.next.RoundTrip(clone)
}

type runAPI struct {
	conn   *nats.Conn
	key    ed25519.PrivateKey
	ctx    context.Context
	mu     sync.Mutex
	hubs   map[string]*runHub
	sends  map[string]bool
	closed bool
}
type runObservation struct {
	Snapshot               *bonnieclient.Snapshot
	Durable, Live          []runtime.Event
	Unreachable, Truncated bool
	// Observed is set once the hub has made its first snapshot attempt, so a
	// fresh hub's empty state is not mistaken for an empty conversation.
	Observed bool
}
type runHub struct {
	mu      sync.Mutex
	state   runObservation
	viewers int
	cancel  context.CancelFunc
	done    chan struct{}
}

func newRunAPI(ctx context.Context, nc *nats.Conn, key ed25519.PrivateKey) *runAPI {
	return &runAPI{conn: nc, key: key, ctx: ctx, hubs: make(map[string]*runHub), sends: make(map[string]bool)}
}
func (p *runAPI) client(agent string) (*bonnieclient.Client, *http.Client, func(), error) {
	subject, err := runapitoken.Subject("bonnie", agent)
	if err != nil {
		return nil, nil, nil, err
	}
	tr, err := natsapi.NewTransport(natsapi.Config{Conn: p.conn, Subject: subject})
	if err != nil {
		return nil, nil, nil, err
	}
	hc := &http.Client{Transport: &apiBearer{next: tr, key: p.key, agent: agent}}
	return bonnieclient.New("http://jawa-agent", bonnieclient.WithHTTPClient(hc)), hc, func() { _ = tr.Close() }, nil
}

// Transports are bounded by active hubs and requests and never evicted beneath
// callers. No idle per-agent cache or retained offline transport is needed.
func (p *runAPI) snapshot(ctx context.Context, source runObserver) (*bonnieclient.Snapshot, error) {
	a := source.observationIdentity()
	c, _, close, err := p.client(a.AgentID)
	if err != nil {
		return nil, err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Snapshot(ctx, a.RunID)
}
func (p *runAPI) cancelTurn(ctx context.Context, a Attempt, turn string) (runtime.CancelResult, error) {
	c, _, close, err := p.client(a.AgentID)
	if err != nil {
		return runtime.CancelResult{}, err
	}
	defer close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.RequestCancel(ctx, a.RunID, turn)
}
func runKey(a runObserver) string { id := a.observationIdentity(); return id.AgentID + "/" + id.RunID }
func (p *runAPI) reserveSend(a Attempt) (func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := runKey(a)
	if p.closed || p.sends[k] || len(p.sends) >= 64 {
		return nil, false
	}
	p.sends[k] = true
	return func() { p.mu.Lock(); delete(p.sends, k); p.mu.Unlock() }, true
}
func (p *runAPI) observe(source runObserver) (*runHub, func(), error) {
	a := source.observationIdentity()
	// A tab switch can arrive while the previous viewer's stream is closing.
	// Wait boundedly for that same hub, rather than returning a finite SSE error
	// or overlapping upstream streams outside the capacity accounting.
	waitCtx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()
	for {
		p.mu.Lock()
		stopping := p.hubs[runKey(a)]
		if stopping == nil || stopping.viewers > 0 {
			break
		}
		p.mu.Unlock()
		select {
		case <-stopping.done:
		case <-waitCtx.Done():
			return nil, nil, errRunBusy
		}
	}

	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, errRunBusy
	}
	k := runKey(a)
	h := p.hubs[k]
	if h != nil {
		if h.viewers == 0 || h.viewers >= 64 {
			return nil, nil, errRunBusy
		}
		h.viewers++
	} else {
		count := 0
		for key := range p.hubs {
			if len(key) > len(a.AgentID) && key[:len(a.AgentID)+1] == a.AgentID+"/" {
				count++
			}
		}
		if count >= 16 || len(p.hubs) >= 256 {
			return nil, nil, errRunBusy
		}
		ctx, cancel := context.WithCancel(p.ctx)
		h = &runHub{viewers: 1, cancel: cancel, done: make(chan struct{}), state: runObservation{Unreachable: true}}
		p.hubs[k] = h
		go p.runHub(ctx, a, h)
	}
	var once sync.Once
	return h, func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			h.viewers--
			if h.viewers == 0 {
				h.cancel() /* retain until stopped, preserving upstream capacity */
			}
		})
	}, nil
}
func (h *runHub) read() runObservation {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.state
	out.Durable = append([]runtime.Event(nil), out.Durable...)
	out.Live = append([]runtime.Event(nil), out.Live...)
	return out
}
func durableEvent(t string) bool {
	switch t {
	case runtime.EventTurn, runtime.EventState, runtime.EventSuspend, runtime.EventResume, runtime.EventResponse, runtime.EventCancelRequested:
		return true
	}
	return false
}

// maxRunEvents bounds each observed event source.
const maxRunEvents = 256

func boundedEvents(events []runtime.Event, ev runtime.Event) ([]runtime.Event, bool) {
	// Bound both number and individual payload size.
	omitted := len(ev.Text) > 64<<10 || len(ev.Data) > 64<<10
	if len(ev.Data) > 64<<10 {
		ev.Data = nil
		ev.Text = "Activity payload omitted (size limit)"
	}
	if len(ev.Text) > 64<<10 {
		ev.Text = ev.Text[:64<<10]
	}
	events = append(events, ev)
	if len(events) > maxRunEvents {
		return events[len(events)-maxRunEvents:], true
	}
	return events, omitted
}
func (p *runAPI) runHub(ctx context.Context, source runObserver, h *runHub) {
	a := source.observationIdentity()
	defer close(h.done)
	defer func() {
		h.mu.Lock()
		h.state.Observed = true
		h.mu.Unlock()
	}()
	defer func() {
		p.mu.Lock()
		if p.hubs[runKey(a)] == h {
			delete(p.hubs, runKey(a))
		}
		p.mu.Unlock()
	}()
	c, _, close, err := p.client(a.AgentID)
	if err != nil {
		return
	}
	defer close()
	cursor := 0
	currentTurn := ""
	backoff := time.Second
	for ctx.Err() == nil {
		snapCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snap, err := c.Snapshot(snapCtx, a.RunID)
		cancel()
		if err == nil {
			h.mu.Lock()
			h.state.Snapshot = snap
			h.state.Observed = true
			currentTurn = snap.TurnID
			h.state.Live = nil
			h.mu.Unlock()
			// Replay historical durable activity on the single upstream stream.
			events, stop, streamErr := c.Stream(ctx, a.RunID, cursor)
			if streamErr == nil {
				h.mu.Lock()
				h.state.Unreachable = false
				h.mu.Unlock()
				backoff = time.Second
				refresh := time.NewTicker(250 * time.Millisecond)
				dirty := false
			streamLoop:
				for {
					select {
					case <-ctx.Done():
						break streamLoop
					case <-refresh.C:
						if dirty {
							readCtx, end := context.WithTimeout(ctx, 5*time.Second)
							fresh, e := c.Snapshot(readCtx, a.RunID)
							end()
							if e != nil {
								break streamLoop
							}
							h.mu.Lock()
							h.state.Snapshot = fresh
							// Committed snapshot supersedes pending deltas, not durable history.
							live := h.state.Live[:0]
							for _, ev := range h.state.Live {
								if ev.TurnID == fresh.TurnID && ev.Seq > fresh.Cursor {
									live = append(live, ev)
								}
							}
							h.state.Live = live
							h.mu.Unlock()
							dirty = false
						}
					case ev, ok := <-events:
						if !ok {
							break streamLoop
						}
						if ev.RunID != a.RunID {
							continue
						}
						h.mu.Lock()
						if durableEvent(ev.Type) {
							if ev.Seq > cursor {
								if ev.Type == runtime.EventTurn {
									currentTurn = ev.TurnID
									h.state.Live = nil
								}
								var truncated bool
								h.state.Durable, truncated = boundedEvents(h.state.Durable, ev)
								h.state.Truncated = h.state.Truncated || truncated
								cursor = ev.Seq
								if ev.Seq > snap.Cursor {
									dirty = true
								}
							}
						} else {
							// Equal sequence live events are distinct and must not be deduplicated.
							if h.state.Snapshot != nil && ev.TurnID == currentTurn && ev.Seq >= h.state.Snapshot.Cursor {
								var truncated bool
								h.state.Live, truncated = boundedEvents(h.state.Live, ev)
								h.state.Truncated = h.state.Truncated || truncated
							}
							if ev.Type == string(kit.EventStepFinish) {
								dirty = true
							}
						}
						h.mu.Unlock()
					}
				}
				refresh.Stop()
				stop()
			}
		}
		h.mu.Lock()
		h.state.Unreachable = true
		h.state.Observed = true
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}
func (p *runAPI) Close() {
	p.mu.Lock()
	p.closed = true
	hubs := make([]*runHub, 0, len(p.hubs))
	for _, h := range p.hubs {
		h.cancel()
		hubs = append(hubs, h)
	}
	p.mu.Unlock()
	for _, h := range hubs {
		<-h.done
	}
}
