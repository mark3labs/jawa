package orchestrator

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	channel "github.com/mark3labs/bonnie/channel/nats"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportbridge"
	"jawa/internal/reportprotocol"
	"jawa/internal/testmodel"
)

// No model/network provider: an exhausted real KIT provider fails, then the
// same SQLite run is restored on a new host and continues with a healthy model.
func TestFailedResumeRealRuntimeBothModes(t *testing.T) {
	for _, mode := range []string{"build", "planning", "fix", "auto-build", "auto-planning", "auto-fix", "unblock", "repeat-unblock"} {
		explicit := strings.Contains(mode, "unblock")
		planning := strings.Contains(mode, "planning")
		automatic := strings.HasPrefix(mode, "auto-")
		repair := strings.HasSuffix(mode, "fix")
		name := mode
		t.Run(name, func(t *testing.T) {
			s, nc := workflowFixture(t)
			if err := s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
				t.Fatal(err)
			}
			card := workflowCard(t, s)
			var repairAssignment Attempt
			if repair {
				seed := &Workflow{s: s, ctx: t.Context()}
				repairDone(t, seed, card)
				if err := seed.FixCard(card.ID, "repair-message"); err != nil {
					t.Fatal(err)
				}
				var e error
				repairAssignment, e = seed.CardResult(card.ID)
				if e != nil {
					t.Fatal(e)
				}
			}
			w, err := NewWorkflow(t.Context(), s, nc)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			dir := filepath.Join(t.TempDir(), "journal")
			var journal *runtime.SQLiteJournal
			var runner *runtime.Runner
			var ch bonnie.Channel
			start := func(model *testmodel.Model) {
				journal, err = runtime.OpenSQLiteJournal(dir)
				if err != nil {
					t.Fatal(err)
				}
				b := &reportbridge.Bridge{}
				factory := runtime.KitAgentWithSetup(false, func(ctx context.Context, k *kit.Kit, session *runtime.Session) error {
					return b.Setup(ctx, k, bonnie.RunScope{RunID: session.RunID(), Session: session})
				}, model.Option(), func(o *kit.Options) {
					o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.DisableCoreTools, o.Quiet = true, true, true, true, true, true, true
				})
				runner = runtime.NewRunner(journal, factory)
				ch, err = b.New(runner, channel.Config{Conn: nc, RootSubject: "bonnie", AgentID: "resume-agent", CreateStream: true, TargetedTasks: true})
				if err != nil {
					t.Fatal(err)
				}
				if err = ch.(interface{ Start(context.Context) error }).Start(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			stop := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if e := ch.(interface{ Shutdown(context.Context) error }).Shutdown(ctx); e != nil {
					t.Fatal(e)
				}
				if e := journal.Close(); e != nil {
					t.Fatal(e)
				}
			}
			var explicitModel *testmodel.Model
			if explicit {
				replies := []testmodel.Reply{testmodel.Call("reportBlocker", `{"reason":"need operator help"}`)}
				if mode == "repeat-unblock" {
					replies = append(replies, testmodel.Call("reportBlocker", `{"reason":"still need help"}`))
				}
				replies = append(replies, testmodel.Call("completeTask", `{"pr_number":7}`))
				explicitModel = testmodel.New(replies...)
				start(explicitModel)
			} else {
				start(testmodel.New())
			}
			lane := "Building"
			if planning {
				lane = "Planning"
			}
			if !repair {
				if err = w.MoveCard(card.ID, lane, 0); err != nil {
					t.Fatal(err)
				}
			}
			expectedNumber := 1
			if repair {
				expectedNumber = repairAssignment.Number
			}
			var task, run, attempt, taskJSON, session string
			waitWorkflow(t, func() bool {
				if planning {
					e := s.db.QueryRow(`SELECT t.id,t.run_id,t.remote_attempt_id,t.task_json,t.session_id FROM planning_turns t JOIN planning_sessions p ON p.id=t.session_id WHERE p.card_id=? AND t.state='failed' AND t.outcome_json<>''`, card.ID).Scan(&task, &run, &attempt, &taskJSON, &session)
					return e == nil
				}
				a, e := w.CardResult(card.ID)
				if e == nil && (executionFailure(a) || (explicit && a.State == "blocked" && a.RunState == "completed")) {
					task, run, attempt, taskJSON = a.TaskID, a.RunID, a.RemoteAttemptID, a.TaskJSON
					return true
				}
				return false
			})
			if explicit {
				waitWorkflow(t, func() bool {
					status, e := w.client.Status(t.Context(), client.Target{TaskID: task, AgentID: "resume-agent", RunID: run, AttemptID: attempt})
					return e == nil && status.State == runtime.RunCompleted && !status.Active
				})
			}
			if explicit {
				waitWorkflow(t, func() bool {
					js, e := nc.JetStream()
					if e != nil {
						return false
					}
					info, e := js.StreamInfo(channel.DefaultResultStreamName("bonnie.results"))
					return e == nil && info.State.Msgs > 0
				})
			}
			// Additional saved context must survive process restart and continuation.
			contextJSON := []byte(`{"context":["saved-context-marker"]}`)
			if _, err = journal.Append(t.Context(), runtime.Record{RunID: run, Kind: runtime.RecordContext, Payload: contextJSON, Text: "saved-context-marker"}); err != nil {
				t.Fatal(err)
			}
			stop()
			if err = w.Close(); err != nil {
				t.Fatal(err)
			}
			w, err = NewWorkflow(t.Context(), s, nc)
			if err != nil {
				t.Fatal(err)
			}
			reply := testmodel.Call("completeTask", `{"pr_number":7}`)
			if repair {
				reply = testmodel.Call("completeTask", `{"pr_number":18}`)
			}
			if planning {
				reply = testmodel.Call("clarify", `{"questions":["Continue with API v2?"]}`)
			}
			if mode == "repeat-unblock" {
				reply = testmodel.Call("reportBlocker", `{"reason":"still need help"}`)
			}
			healthy := testmodel.New(reply)
			if explicit {
				healthy = explicitModel
			}
			beforeRequests := len(healthy.Requests())
			start(healthy)
			action := w.ResumeFailed
			if explicit {
				action = func(id string) error {
					a, e := w.CardResult(id)
					if e != nil {
						return e
					}
					return w.Unblock(id, unblockToken(a, "Building"), "operator-continuation-marker")
				}
			}
			if planning {
				action = w.ResumeFailedPlanning
			}
			if automatic {
				if err = s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "true"}); err != nil {
					t.Fatal(err)
				}
				now := time.Now()
				if err = w.discoverAutoResumes(now); err != nil {
					t.Fatal(err)
				}
				w.autoResumePass(now.Add(81 * time.Second))
			} else {
				var wg sync.WaitGroup
				results := make(chan error, 2)
				gate := make(chan struct{})
				for range 2 {
					wg.Go(func() { <-gate; results <- action(card.ID) })
				}
				close(gate)
				wg.Wait()
				close(results)
				successes := 0
				for e := range results {
					if e != nil {
						t.Logf("resume reservation: %v", e)
					}
					if e == nil {
						successes++
					}
				}
				if successes != 1 {
					t.Fatalf("concurrent resume successes=%d", successes)
				}

			}
			waitWorkflow(t, func() bool {
				var n int
				_ = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE task_id=? AND status='completed'`, task).Scan(&n)
				return n == 1
			})
			var raw string
			if err = s.db.QueryRow(`SELECT request_json FROM failed_resumes WHERE task_id=?`, task).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var req reportprotocol.FailedResume
			_ = json.Unmarshal([]byte(raw), &req)
			// Lost response / explicit replay must return the durable receipt, not run KIT.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			duplicate, e := reportprotocol.ResumeFailed(ctx, nc, "bonnie", req)
			if e != nil || duplicate.Status != "completed" {
				t.Fatal(duplicate, e)
			}
			if len(healthy.Requests()) != beforeRequests+1 {
				t.Fatalf("duplicate executed provider: %d", len(healthy.Requests()))
			}
			text := healthy.Requests()[beforeRequests].Text()
			if !strings.Contains(text, "saved-context-marker") {
				t.Fatal("saved context lost", text)
			}
			var original client.Task
			_ = json.Unmarshal([]byte(taskJSON), &original)
			if strings.Count(text, original.Text) != 1 {
				t.Fatal("original prompt duplicated/lost", text)
			}
			if planning {
				d := planningDetails(t, w, card.ID)
				if len(d.Turns) != 1 || d.Turns[0].ID != task || d.Session.ID != session || d.Session.State != "questions" {
					t.Fatal("planning context/identity changed", d)
				}
				var gotRun, gotAttempt string
				_ = s.db.QueryRow(`SELECT run_id,remote_attempt_id FROM planning_turns WHERE id=?`, task).Scan(&gotRun, &gotAttempt)
				if gotRun != run || gotAttempt != attempt {
					t.Fatal("remote identity changed")
				}
			} else {
				a, e := w.CardResult(card.ID)
				if e != nil || a.TaskID != task || a.RunID != run || a.RemoteAttemptID != attempt || a.TaskJSON != taskJSON || a.Number != expectedNumber || (!explicit && a.PRURL == "") {
					t.Fatal("build identity/branch/report changed", a, e)
				}
			}
			if repair {
				a, e := w.CardResult(card.ID)
				if e != nil || a.Branch != repairAssignment.Branch || a.RepairMessage != repairAssignment.RepairMessage || a.RepairPRURL != repairAssignment.RepairPRURL || a.RepairSourceNumber != repairAssignment.RepairSourceNumber {
					t.Fatal("repair assignment lost", a, e)
				}
			}
			if explicit {
				a, e := w.CardResult(card.ID)
				if e != nil || a.ReportGeneration != 1 {
					t.Fatal(a, e)
				}
				if strings.Count(text, "operator-continuation-marker") != 1 {
					t.Fatal("operator prompt lost/duplicated", text)
				}
				var count int
				if e = s.db.QueryRow(`SELECT count(*) FROM typed_reports WHERE task_id=?`, task).Scan(&count); e != nil || count != 2 {
					t.Fatal("report history lost", count, e)
				}
			}
			// Old terminal outcome and event cannot undo the new typed state.
			var previous string
			_ = s.db.QueryRow(`SELECT previous_outcome FROM failed_resumes WHERE task_id=?`, task).Scan(&previous)
			var old client.Outcome
			_ = json.Unmarshal([]byte(previous), &old)
			if e = w.result(t.Context(), old); e != nil {
				t.Fatal(e)
			}
			if e = w.event(t.Context(), client.StatusEvent{Version: 1, Target: req.Target, EventID: "stale-failure", Seq: req.Seq, State: runtime.RunFailed}); e != nil {
				t.Fatal(e)
			}
			if e = w.ResumeFailed(card.ID); e == nil {
				t.Fatal("resumed nonfailure again")
			}
			// Host journal receipt survives host restart too.
			stop()
			start(healthy)
			duplicate, e = reportprotocol.ResumeFailed(ctx, nc, "bonnie", req)
			if e != nil || duplicate.Status != "completed" || len(healthy.Requests()) != beforeRequests+1 {
				t.Fatal("restart dedup lost", duplicate, e)
			}
			if mode == "repeat-unblock" {
				stop()
				final := explicitModel
				start(final)
				if err = action(card.ID); err != nil {
					t.Fatal(err)
				}
				waitWorkflow(t, func() bool {
					a, e := w.CardResult(card.ID)
					return e == nil && a.ReportGeneration == 2 && a.PRURL != ""
				})
			}
			if explicit && mode != "repeat-unblock" {
				a, e := w.CardResult(card.ID)
				if e != nil || a.Ready {
					t.Fatal("verification was bypassed", a, e)
				}
				w.SetVerifier(func(context.Context, Attempt) (bool, error) { return true, nil })
				w.verifyPending()
				a, e = w.CardResult(card.ID)
				if e != nil || !a.Ready {
					t.Fatal("normal verification failed", a, e)
				}
			}
			if planning && automatic {
				detail := planningDetails(t, w, card.ID)
				if err = w.ReplyPlanning(card.ID, "Use API v2", detail.InteractionToken); err != nil {
					t.Fatal(err)
				}
				var count int
				if err = s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE task_id=? AND automatic=1`, task).Scan(&count); err != nil || count != 1 {
					t.Fatal("Answer reset automatic budget", count, err)
				}
				next := planningDetails(t, w, card.ID)
				if len(next.Turns) != 1 || next.Turns[0].ID != task {
					t.Fatal("Answer replaced task budget", next)
				}
			}

		})
	}
}
