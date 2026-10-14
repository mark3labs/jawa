package orchestrator

import (
	"context"
	"crypto/ed25519"
	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	api "github.com/mark3labs/bonnie/channel/natsapi"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/runapitoken"
	"jawa/internal/testmodel"
	"net/http"
	"testing"
	"time"
)

func TestRunAPIRealSnapshotSharedReplayAndFollowup(t *testing.T) {
	s, nc := workflowFixture(t)
	key, err := s.apiSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	journal, err := runtime.OpenSQLiteJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	model := testmodel.New(testmodel.Say("initial response"), testmodel.Say("follow-up response"))
	runner := runtime.NewRunner(journal, runtime.KitAgentWithoutHumanInput(model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
	}))
	run, err := runner.Start(t.Context(), "api-fixture-run", runtime.Input{Text: "initial prompt"})
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := runapitoken.Subject("bonnie", "api-test-agent")
	bridge, err := api.New(runner, api.Config{Conn: nc, Subject: subject, Authenticator: func(r *http.Request) (*channel.Principal, error) {
		token := r.Header.Get("Authorization")
		if len(token) < 7 || runapitoken.Verify(key.Public().(ed25519.PublicKey), token[7:], "api-test-agent", time.Now()) != nil {
			return nil, bonniehttp.ErrUnauthenticated
		}
		return &channel.Principal{ID: "orchestrator", Kind: "user", Authenticator: "test"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = bridge.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bridge.Shutdown(ctx)
	}()
	pool := newRunAPI(t.Context(), nc, key)
	defer pool.Close()
	a := Attempt{AgentID: "api-test-agent", RunID: run.ID}
	snap, err := pool.snapshot(t.Context(), a)
	if err != nil || len(snap.Messages) == 0 {
		t.Fatal("snapshot", err)
	}
	h, release, err := pool.observe(a)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// A pinned planner uses the same read-only snapshot and shared hub adapter.
	planner := RunRecord{Mode: "Planning", AgentID: a.AgentID, RunID: a.RunID, RemoteAttemptID: "remote"}
	if snap, err := pool.snapshot(t.Context(), planner); err != nil || len(snap.Messages) == 0 {
		t.Fatal("planning snapshot", err)
	}
	h2, release2, err := pool.observe(planner)
	if err != nil || h2 != h {
		t.Fatal("observation not shared", err)
	}
	defer release2()
	waitWorkflow(t, func() bool { v := h.read(); return !v.Unreachable && len(v.Durable) > 0 })
	if err = pool.send(t.Context(), a, "follow-up", "queue"); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool { v := h.read(); return v.Snapshot != nil && v.Snapshot.Response == "follow-up response" })
	seen := map[int]bool{}
	for _, ev := range h.read().Durable {
		if seen[ev.Seq] {
			t.Fatal("duplicate durable event", ev.Seq)
		}
		seen[ev.Seq] = true
	}
}
