package reportbridge

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"jawa/internal/agentconcurrency"
	"jawa/internal/testmodel"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

type gateTurn struct {
	ctx    context.Context
	finish chan struct{}
}

// Use BONNIE's managed host factory (including its completion wrapper), not
// KitAgentWithSetup or a fake runtime.Agent. Setup must receive the actual
// turn context, which Runner cancels even on ordinary successful completion.
func TestManagedHostConcurrencyLifecycle(t *testing.T) {
	var b Bridge
	b.SetConcurrency(1)
	var turns sync.Map
	attempted := make(chan string, 16)
	entered := make(chan string, 16)
	results := make(chan struct {
		id  string
		run *runtime.Run
		err error
	}, 16)
	model := testmodel.New(testmodel.Say("done"), testmodel.Say("done"), testmodel.Say("done"), testmodel.Say("done"))
	a := bonnie.New(
		bonnie.WithJournal(t.TempDir()), bonnie.WithInstructions(""), bonnie.WithContextFiles(""), bonnie.WithSkills(""), bonnie.WithoutHumanInput(),
		bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(t.TempDir()))),
		bonnie.WithKit(model.Option(), func(o *kit.Options) {
			o.SkipConfig, o.NoContextFiles, o.NoSkills = true, true, true
			o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true
		}),
		bonnie.WithKitSetup(func(ctx context.Context, k *kit.Kit, scope bonnie.RunScope) error {
			attempted <- scope.RunID
			if err := b.Setup(ctx, k, scope); err != nil {
				return err
			}
			turn := &gateTurn{ctx: ctx, finish: make(chan struct{})}
			turns.Store(scope.RunID, turn)
			entered <- scope.RunID
			k.OnPrepareStep(kit.HookPriorityNormal, func(kit.PrepareStepHook) *kit.PrepareStepResult {
				select {
				case <-turn.finish:
				case <-ctx.Done():
				}
				return nil
			})
			return nil
		}),
		bonnie.WithCompletionHook(bonnie.CompletionPolicy{NewHook: func(context.Context, bonnie.RunScope) (bonnie.CompletionHook, error) {
			return func(context.Context, bonnie.CompletionCandidate) (bonnie.CompletionFeedback, error) {
				return bonnie.CompletionFeedback{}, nil
			}, nil
		}}),
	)
	err := a.WithRuntime(t.Context(), func(hostCtx context.Context, rt *bonnie.Runtime) error {
		r := rt.Runner()
		// Always unblock all durable turns on a test failure; caller cancellation
		// intentionally does not cancel BONNIE's durable execution contexts.
		defer func() {
			for _, id := range []string{"one", "two", "three", "waiting", "after"} {
				_ = r.Cancel(id)
			}
		}()
		start := func(id string) {
			go func() {
				run, err := r.Start(hostCtx, id, runtime.Input{Text: "say done"})
				results <- struct {
					id  string
					run *runtime.Run
					err error
				}{id, run, err}
			}()
			if got := receive(t, attempted); got != id {
				t.Fatalf("setup attempted %q, want %q", got, id)
			}
		}
		admit := func(id string) *gateTurn {
			t.Helper()
			if got := receive(t, entered); got != id {
				t.Fatalf("admitted %q, want %q", got, id)
			}
			v, _ := turns.Load(id)
			return v.(*gateTurn)
		}
		done := func(id string, state runtime.RunState) {
			t.Helper()
			got := receive(t, results)
			if got.err != nil || got.id != id || got.run == nil || got.run.State != state {
				t.Fatalf("result %s: %+v, %v; want %s/%s", got.id, got.run, got.err, id, state)
			}
		}
		blocked := func() {
			t.Helper()
			select {
			case id := <-entered:
				t.Fatalf("unexpected admission %s", id)
			case <-time.After(100 * time.Millisecond):
			}
		}
		start("one")
		one := admit("one")
		start("two")
		blocked()
		close(one.finish)
		done("one", runtime.RunCompleted)
		select {
		case <-one.ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("Setup context was not canceled on completed turn")
		}
		if hostCtx.Err() != nil {
			t.Fatal("host context canceled with turn")
		}
		two := admit("two") // ordinary completion must release, not just cancellation
		start("three")
		blocked()
		b.gate.Set(2)
		three := admit("three") // live increase wakes an already waiting Setup
		b.gate.Set(1)
		start("waiting")
		blocked()
		if err := r.Cancel("waiting"); err != nil {
			return err
		}
		done("waiting", runtime.RunCancelled) // cancellation while inside Acquire
		blocked()
		if err := r.Cancel("two"); err != nil {
			return err
		}
		done("two", runtime.RunCancelled)
		select {
		case <-two.ctx.Done():
		default:
			t.Fatal("canceled holder context still live")
		}
		start("after")
		blocked() // shrinking does not evict three or admit past the new limit
		close(three.finish)
		done("three", runtime.RunCompleted)
		after := admit("after")
		close(after.finish)
		done("after", runtime.RunCompleted)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := b.gate.Acquire(ctx); err != nil {
			return fmt.Errorf("slot leaked: %w", err)
		}
		b.gate.Release()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for managed turn")
		var zero T
		return zero
	}
}

// Setup failures must release immediately, even if their caller has not yet
// canceled the context. Replay is the first fallible operation after Acquire.
func TestSetupFailureReleasesConcurrency(t *testing.T) {
	var b Bridge
	b.SetConcurrency(1)
	b.runner = runtime.NewRunner(failingReplayJournal{Journal: runtime.NewMemoryJournal()}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := b.Setup(ctx, nil, bonnie.RunScope{RunID: "broken"}); !errors.Is(err, errReplay) {
		t.Fatalf("Setup = %v", err)
	}
	acquireCtx, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	if err := b.gate.Acquire(acquireCtx); err != nil {
		t.Fatalf("failed setup leaked slot: %v", err)
	}
	b.gate.Release()
	cancel() // must not double-release after the error-path release
	b.gate.Set(agentconcurrency.Default)
}

var errReplay = errors.New("replay failed")

type failingReplayJournal struct{ runtime.Journal }

func (f failingReplayJournal) Replay(context.Context, string) ([]runtime.Record, error) {
	return nil, errReplay
}

// Failed channel startup must stop the poller even when the host never calls
// Shutdown, and must not retain a connection advertised as usable by tools.
func TestStartFailureStopsConcurrency(t *testing.T) {
	s, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	defer s.Shutdown()
	if !s.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	nc, err := nats.Connect(s.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for _, restoreFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore_failure_%t", restoreFailure), func(t *testing.T) {
			var b Bridge
			b.SetConcurrency(1)
			if restoreFailure {
				b.runner = runtime.NewRunner(failingRunsJournal{Journal: runtime.NewMemoryJournal()}, nil)
			}
			underlying := &failedStartChannel{}
			ch := &reportChannel{Channel: underlying, bridge: &b, cfg: natschannel.Config{Conn: nc}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := ch.Start(ctx); err == nil {
				t.Fatal("Start unexpectedly succeeded")
			}
			stopped := make(chan struct{})
			go func() { ch.concurrencyWG.Wait(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(3 * time.Second):
				t.Fatal("failed Start leaked policy goroutine")
			}
			if b.conn != nil {
				t.Fatal("failed Start retained bridge connection")
			}
			if nc.IsClosed() {
				t.Fatal("closed caller-owned connection")
			}
		})
	}
}

type failedStartChannel struct{ bonnie.Channel }

func (*failedStartChannel) Start(context.Context) error { return errors.New("channel start failed") }

type failingRunsJournal struct{ runtime.Journal }

func (f failingRunsJournal) Runs(context.Context, runtime.RunState) ([]string, error) {
	return nil, errReplay
}
