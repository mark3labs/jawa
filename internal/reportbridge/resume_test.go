package reportbridge

import (
	"context"
	"encoding/json"
	"testing"

	channel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

func TestFailedResumeHostFailClosed(t *testing.T) {
	ctx := context.Background()
	j := runtime.NewMemoryJournal()
	r := runtime.NewRunner(j, nil)
	cfg := channel.Config{RootSubject: "bonnie", AgentID: "agent"}
	b := &Bridge{runner: r, cfg: cfg}
	target := client.Target{TaskID: "task", AgentID: "agent", RunID: "run", AttemptID: "attempt"}
	admission, _ := json.Marshal(target)
	_, _ = j.Append(ctx, runtime.Record{RunID: admissionKey(cfg), Kind: runtime.RecordExtensionData, ExtType: "nats_task_admitted", Payload: admission})
	_, _ = j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordTurn, Text: "turn"})
	seq, _ := j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordState, State: runtime.RunFailed})
	valid := reportprotocol.FailedResume{Version: 1, ID: "resume", Target: target, TurnID: "turn", Seq: seq}
	for name, edit := range map[string]func(*reportprotocol.FailedResume){
		"stale cursor":  func(q *reportprotocol.FailedResume) { q.Seq-- },
		"stale turn":    func(q *reportprotocol.FailedResume) { q.TurnID = "old" },
		"unknown task":  func(q *reportprotocol.FailedResume) { q.Target.TaskID = "other" },
		"wrong agent":   func(q *reportprotocol.FailedResume) { q.Target.AgentID = "other" },
		"wrong attempt": func(q *reportprotocol.FailedResume) { q.Target.AttemptID = "other" },
		"version":       func(q *reportprotocol.FailedResume) { q.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			q := valid
			edit(&q)
			reply := b.admitResume(ctx, q)
			if reply.Status != "rejected" || reply.Error == "" {
				t.Fatal(reply)
			}
		})
	}
	// Crash after intent but before StartRetry is deliberately ambiguous. Neither
	// replay nor a new command identity may start the saved failed run blindly.
	raw, _ := json.Marshal(valid)
	_, _ = j.Append(ctx, runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: resumeIntentType, Payload: raw})
	reply := b.admitResume(ctx, valid)
	if reply.Status != "pending" {
		t.Fatal(reply)
	}
	changed := valid
	changed.ID = "new"
	changed.Seq++
	reply = b.admitResume(ctx, changed)
	if reply.Status != "rejected" {
		t.Fatal("ambiguous prior intent was bypassed", reply)
	}
	reply = b.admitResume(ctx, valid)
	if reply.Status != "pending" || r.IsActive("run") {
		t.Fatal("replay executed ambiguous command", reply)
	}
}
