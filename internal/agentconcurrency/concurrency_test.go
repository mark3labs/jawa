package agentconcurrency

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, n := range []int{1, Default, Max} {
		got, err := Parse(strconv.Itoa(n))
		if err != nil || got != n {
			t.Fatalf("Parse(%d) = %d, %v", n, got, err)
		}
	}
	for _, value := range []string{"", "0", "-1", "1025", "1.5", " 4", "4 ", "no"} {
		if _, err := Parse(value); err == nil {
			t.Errorf("Parse(%q) accepted invalid policy", value)
		}
	}
}

func TestGateResizeCancel(t *testing.T) {
	g := New(2)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		if err := g.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	g.Set(1)
	waitingCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- g.Acquire(waitingCtx) }()
	g.Release()
	select {
	case err := <-done:
		t.Fatalf("shrink admitted waiter: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	go func() { done <- g.Acquire(ctx) }()
	g.Set(2)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	g.Release()
	g.Release()
	// Canceled contexts cannot consume even an immediately available slot.
	if err := g.Acquire(waitingCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		t.Fatalf("active = %d, want 0", g.active)
	}
}

func TestGateConcurrentResize(t *testing.T) {
	g := New(Default)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 100 {
				if err := g.Acquire(ctx); err != nil {
					t.Error(err)
					return
				}
				g.Release()
			}
		})
	}
	wg.Go(func() {
		for i := range 1000 {
			g.Set(1 + i%8)
		}
	})
	wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 {
		t.Fatalf("active = %d, want 0", g.active)
	}
}
