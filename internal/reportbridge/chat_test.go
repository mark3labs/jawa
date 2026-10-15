package reportbridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportprotocol"
	"jawa/internal/testmodel"
)

func TestChatHostContinuationDedupAndFollowup(t *testing.T) {
	for _, state := range []runtime.RunState{runtime.RunFailed, runtime.RunCancelled, runtime.RunCompleted, runtime.RunInterrupted} {
		t.Run(string(state), func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, resume := resumeFixture(t, j)
			if _, err := j.Append(t.Context(), runtime.Record{RunID: "run", Kind: runtime.RecordState, State: state}); err != nil {
				t.Fatal(err)
			}
			model := testmodel.New(testmodel.Say("chat response"))
			b.runner = runtime.NewRunner(j, runtime.KitAgentWithoutHumanInput(model.Option(), func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
			}))
			records, _ := j.Replay(t.Context(), "run")
			cmd := reportprotocol.Chat{Version: 1, ID: "chat", Target: resume.Target, Text: "explicit new text", TurnID: "turn", Seq: records[len(records)-1].Seq, Followup: true}
			got := b.admitChat(t.Context(), cmd)
			if got.Status != "active" {
				t.Fatal(got)
			}
			b.resumeWG.Wait()
			got = b.admitChat(t.Context(), cmd)
			if got.Status != "completed" || got.Outcome == nil {
				t.Fatal(got)
			}
			// Restart retains receipt, no second execution even with another runner.
			b2 := &Bridge{runner: runtime.NewRunner(j, nil), cfg: b.cfg}
			if again := b2.admitChat(t.Context(), cmd); again.Status != "completed" {
				t.Fatal(again)
			}
			messages, err := runtime.Restore(t.Context(), "run", j)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, m := range messages.GetMessages() {
				if m.Role == "user" {
					for _, p := range m.Content {
						if text, ok := p.(kit.LLMTextPart); ok && text.Text == cmd.Text {
							count++
						}
					}
				}
			}
			if count != 1 {
				t.Fatalf("new message count %d", count)
			}
			records, _ = j.Replay(t.Context(), "run")
			if !chatFollowup(records) {
				t.Fatal("missing followup provenance")
			}
			if _, err = b2.identity(t.Context(), "run"); err == nil || !strings.Contains(err.Error(), "authority") {
				t.Fatal(err)
			}
			changed := cmd
			changed.Text = "different"
			if reply := b2.admitChat(t.Context(), changed); reply.Failure != "identity_conflict" {
				t.Fatal(reply)
			}
		})
	}
}

func TestChatHostOrphanAndRetiredFailClosed(t *testing.T) {
	for _, scenario := range []string{"orphan", "retired", "stale", "unknown"} {
		t.Run(scenario, func(t *testing.T) {
			j := runtime.NewMemoryJournal()
			b, resume := resumeFixture(t, j)
			cmd := reportprotocol.Chat{Version: 1, ID: "chat", Target: resume.Target, Text: "new text", TurnID: resume.TurnID, Seq: resume.Seq, Followup: true}
			want := "failed"
			switch scenario {
			case "orphan":
				raw, _ := json.Marshal(cmd)
				_, _ = j.Append(t.Context(), runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: chatIntentType, Payload: raw})
				want = "ambiguous"
			case "retired":
				_, _ = j.Append(t.Context(), runtime.Record{RunID: "run", Kind: runtime.RecordState, State: runtime.RunRetired})
			case "stale":
				cmd.Seq--
				want = "queued"
			case "unknown":
				cmd.Target.TaskID = "unknown"
			}
			got := b.admitChat(t.Context(), cmd)
			if got.Status != want || b.runner.IsActive("run") {
				t.Fatal(got)
			}
			if scenario == "orphan" {
				newer := cmd
				newer.ID = "new-command"
				if got = b.admitChat(t.Context(), newer); got.Status != "queued" || got.Failure != "prior_command_unconfirmed" {
					t.Fatal(got)
				}
				cmd.Reconcile = true
				if got = b.admitChat(t.Context(), cmd); got.Status != "failed" || got.Failure != "reconciled_not_replayed" {
					t.Fatal(got)
				}
				cmd.Reconcile = false
				if got = b.admitChat(t.Context(), cmd); got.Status != "failed" || b.runner.IsActive("run") {
					t.Fatal("delayed original replayed", got)
				}

			}
		})
	}
}

func TestFollowupSetupCannotReplayOldReportIntent(t *testing.T) {
	j := runtime.NewMemoryJournal()
	b, resume := resumeFixture(t, j)
	cmd := reportprotocol.Chat{Version: 1, ID: "followup", Target: resume.Target, Text: "question", Followup: true}
	raw, _ := json.Marshal(cmd)
	_, _ = j.Append(t.Context(), runtime.Record{RunID: "run", Kind: runtime.RecordExtensionData, ExtType: chatIntentType, Payload: raw})
	if err := b.recoverIntent(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	// Production setup adds fail-closed tools without restoring old authority.
	model := testmodel.New(testmodel.Say("followup"))
	runner := runtime.NewRunner(j, runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, s *runtime.Session) error {
		return b.Setup(ctx, k, bonnie.RunScope{RunID: s.RunID(), Session: s})
	}, model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
	}))
	b.runner = runner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if run, err := runner.Start(ctx, "run", runtime.Input{Text: "followup"}); err != nil || run.State != runtime.RunCompleted {
		t.Fatal(run, err)
	}
}
