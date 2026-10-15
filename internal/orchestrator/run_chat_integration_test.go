package orchestrator

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mark3labs/bonnie"
	channel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportbridge"
	"jawa/internal/testmodel"
)

// Local JetStream, real KIT scripted model, SQLite journals and authenticated
// chat route. The owner is offline at acceptance; no paid/remote provider calls.
func TestRunChatOfflineReconnectBothModes(t *testing.T) {
	for _, mode := range []string{"Build", "Planning"} {
		t.Run(mode, func(t *testing.T) {
			s, nc := workflowFixture(t)
			if err := s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
				t.Fatal(err)
			}
			w, err := NewWorkflow(t.Context(), s, nc)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			card := workflowCard(t, s)
			lane := "Building"
			if mode == "Planning" {
				lane = "Planning"
			}
			if err = w.MoveCard(card.ID, lane, 0); err != nil {
				t.Fatal(err)
			}
			journal, err := runtime.OpenSQLiteJournal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = journal.Close() }()
			var b reportbridge.Bridge
			model := testmodel.New(testmodel.Say("original response"), testmodel.Say("chat response"), testmodel.Say("second response"))
			factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, session *runtime.Session) error {
				return b.Setup(ctx, k, bonnie.RunScope{RunID: session.RunID(), Session: session})
			}, model.Option(), func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
			})
			runner := runtime.NewRunner(journal, factory)
			ch, err := b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "chat-agent", CreateStream: true, TargetedTasks: true})
			if err != nil {
				t.Fatal(err)
			}
			if err = ch.(interface{ Start(context.Context) error }).Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			stopped := false
			stop := func() {
				if !stopped {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if e := ch.(interface{ Shutdown(context.Context) error }).Shutdown(ctx); e != nil {
						t.Fatal(e)
					}
					stopped = true
				}
			}
			defer stop()
			var record RunRecord
			waitWorkflow(t, func() bool {
				if mode == "Build" {
					a, e := w.CardResult(card.ID)
					if e == nil && a.RunID != "" && a.OutcomeJSON != "" {
						record = buildRun(a)
						return true
					}
				} else {
					var id string
					if s.db.QueryRow(`SELECT t.id FROM planning_turns t JOIN planning_sessions p ON p.id=t.session_id WHERE p.card_id=? AND t.outcome_json<>''`, card.ID).Scan(&id) == nil {
						record, _ = w.historyByID(t.Context(), mode, id)
						return record.pinned()
					}
				}
				return false
			})
			stop()
			m, err := w.QueueRunChat(t.Context(), mode, record.ID, uuid.NewString(), "first explicit message", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.QueueRunChat(t.Context(), mode, record.ID, uuid.NewString(), "second explicit message", "")
			if err != nil {
				t.Fatal(err)
			}
			if got := w.chatMessages(record); len(got) != 2 || got[0].Status != "queued" {
				t.Fatal(got)
			}
			// A new owner restores only the retained journal and conversation.
			b = reportbridge.Bridge{}
			runner = runtime.NewRunner(journal, factory)
			ch, err = b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "chat-agent", CreateStream: true, TargetedTasks: true})
			if err != nil {
				t.Fatal(err)
			}
			if err = ch.(interface{ Start(context.Context) error }).Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			stopped = false
			waitWorkflow(t, func() bool {
				rows := w.chatMessages(record)
				return len(rows) == 2 && rows[0].Status == "completed" && rows[1].Status == "completed"
			})
			session, err := runtime.Restore(t.Context(), record.RunID, journal)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			var order []string
			for _, msg := range session.GetMessages() {
				if msg.Role == "user" {
					for _, part := range msg.Content {
						if text, ok := part.(kit.LLMTextPart); ok {
							counts[text.Text]++
							if text.Text == m.Text || text.Text == "second explicit message" {
								order = append(order, text.Text)
							}
						}
					}
				}
			}
			if counts[m.Text] != 1 || counts["second explicit message"] != 1 || len(order) != 2 || order[0] != m.Text {
				raw, _ := json.Marshal(counts)
				t.Fatalf("messages %s order %v", raw, order)
			}
			after, err := w.historyByID(t.Context(), mode, record.ID)
			if err != nil || after.RunID != record.RunID || after.RemoteAttemptID != record.RemoteAttemptID {
				t.Fatal(after, err)
			}
		})
	}
}

func TestRunChatContinuationCanParkForGuidance(t *testing.T) {
	s, nc := workflowFixture(t)
	if err := s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
		t.Fatal(err)
	}
	w, err := NewWorkflow(t.Context(), s, nc)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	card := workflowCard(t, s)
	if err = w.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	journal := runtime.NewMemoryJournal()
	var b reportbridge.Bridge
	model := testmodel.New(testmodel.Say("unreported original completion"), testmodel.Call("reportBlocker", `{"reason":"need guidance after chat"}`), testmodel.Say("continued after guidance"))
	factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, session *runtime.Session) error {
		return b.Setup(ctx, k, bonnie.RunScope{RunID: session.RunID(), Session: session})
	}, model.Option(), func(o *kit.Options) {
		o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
	})
	runner := runtime.NewRunner(journal, factory)
	ch, err := b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "guidance-chat-agent", CreateStream: true, TargetedTasks: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = ch.(interface{ Start(context.Context) error }).Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ch.(interface{ Shutdown(context.Context) error }).Shutdown(ctx)
	}()
	var record RunRecord
	waitWorkflow(t, func() bool {
		a, e := w.CardResult(card.ID)
		if e == nil && a.OutcomeJSON != "" {
			record = buildRun(a)
			return true
		}
		return false
	})
	if _, err = w.QueueRunChat(t.Context(), "Build", record.ID, uuid.NewString(), "continue current task", ""); err != nil {
		t.Fatal(err)
	}
	var token string
	waitWorkflow(t, func() bool {
		var raw string
		e := s.db.QueryRow(`SELECT report_id,waiting_json FROM workflow_interactions WHERE task_id=? AND status='pending'`, record.TaskID).Scan(&token, &raw)
		messages := w.chatMessages(record)
		return e == nil && raw != "" && len(messages) == 1 && messages[0].Status == "completed"
	})
	if _, err = w.QueueRunChat(t.Context(), "Build", record.ID, uuid.NewString(), "exact guidance", token); err != nil {
		t.Fatal(err)
	}
	waitWorkflow(t, func() bool {
		messages := w.chatMessages(record)
		return len(messages) == 2 && messages[1].Status == "completed"
	})
}
