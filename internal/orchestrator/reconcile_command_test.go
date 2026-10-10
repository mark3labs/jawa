package orchestrator

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestReconcileCommandValidation(t *testing.T) {
	flags := []string{"data-dir", "task", "run", "attempt", "nats-url"}
	for _, missing := range flags {
		t.Run(missing, func(t *testing.T) {
			cmd := ReconcileCommand()
			var args []string
			for _, flag := range flags {
				value := "value"
				if flag == missing {
					value = " "
				}
				args = append(args, "--"+flag, value)
			}
			cmd.SetArgs(args)
			if err := cmd.ExecuteContext(t.Context()); err == nil || !strings.Contains(err.Error(), "--"+missing+" must be nonempty") {
				t.Fatalf("validation: %v", err)
			}
		})
	}
}

func TestReconcileCommandRetainedResult(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(map[bool]string{false: "selected", true: "superseded"}[superseded], func(t *testing.T) {
			for _, name := range []string{"NATS_USERNAME", "NATS_PASSWORD", "JAWA_GITHUB_TOKEN", "GITHUB_TOKEN", "JAWA_FORGEJO_TOKEN"} {
				t.Setenv(name, "")
			}
			_, nc := workflowFixture(t)
			dir := t.TempDir()
			s, err := OpenStore(filepath.Join(dir, "orchestrator.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if err = migrateWorkflow(s); err != nil {
				t.Fatal(err)
			}
			w := &Workflow{s: s, ctx: t.Context()}
			card := workflowCard(t, s)
			if err = w.MoveCard(card.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, err := w.CardResult(card.ID)
			if err != nil {
				t.Fatal(err)
			}
			old := client.Outcome{TaskID: a.TaskID, WorkerID: "A", RunID: "A", AttemptID: "A", State: runtime.RunFailed}
			if err = w.result(t.Context(), old); err != nil {
				t.Fatal(err)
			}
			if superseded {
				if err = w.Retry(card.ID); err != nil {
					t.Fatal(err)
				}
			}
			js, err := nc.JetStream()
			if err != nil {
				t.Fatal(err)
			}
			stream := client.DefaultResultStreamName("bonnie.results")
			if _, err = js.AddStream(&nats.StreamConfig{Name: stream, Subjects: []string{"bonnie.results"}}); err != nil {
				t.Fatal(err)
			}
			if _, err = js.AddConsumer(stream, &nats.ConsumerConfig{Durable: "operator-test", AckPolicy: nats.AckExplicitPolicy}); err != nil {
				t.Fatal(err)
			}
			out := client.Outcome{TaskID: a.TaskID, WorkerID: "B", RunID: "B", AttemptID: "B", State: runtime.RunCompleted, Response: "```json\n{\"pr_number\":2}\n```"}
			publishOutcome(t, nc, out)
			before, err := js.ConsumerInfo(stream, "operator-test")
			if err != nil {
				t.Fatal(err)
			}
			// Watch every workflow subject, including untargeted and targeted
			// tasks. No dispatcher is constructed in this fixture or command.
			sub, err := nc.SubscribeSync("bonnie.>")
			if err != nil {
				t.Fatal(err)
			}
			if err = nc.Flush(); err != nil {
				t.Fatal(err)
			}
			cmd := Command()
			var output bytes.Buffer
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"reconcile", "--data-dir", dir, "--task", a.TaskID, "--run", "B", "--attempt", "B", "--nats-url", nc.ConnectedUrl()})
			err = cmd.ExecuteContext(t.Context())
			if superseded {
				if err == nil || !strings.Contains(err.Error(), "latest unready attempt") {
					t.Fatalf("latest guard: %v", err)
				}
			} else if err != nil || !strings.Contains(output.String(), "blocked; provider readiness not verified") {
				t.Fatalf("command: %v %s", err, &output)
			}
			got, err := scanAttempt(s.db.QueryRow(`SELECT `+attemptColumns+` FROM workflow_attempts WHERE task_id=?`, a.TaskID))
			if err != nil {
				t.Fatal(err)
			}
			var audits int
			if err = s.db.QueryRow(`SELECT count(*) FROM workflow_reconciliations`).Scan(&audits); err != nil {
				t.Fatal(err)
			}
			if superseded {
				if got.WorkerID != "A" || audits != 0 {
					t.Fatalf("guard mutated attempt: %+v audits=%d", got, audits)
				}
			} else {
				if got.WorkerID != "B" || got.PRURL != "https://github.com/example/repo/pull/2" || got.State != "blocked" || got.Ready || audits != 1 {
					t.Fatalf("selected: %+v audits=%d", got, audits)
				}
				var previous, selected string
				if err = s.db.QueryRow(`SELECT previous_run_id,selected_run_id FROM workflow_reconciliations`).Scan(&previous, &selected); err != nil || previous != "A" || selected != "B" {
					t.Fatalf("audit: %s %s %v", previous, selected, err)
				}
			}
			after, err := js.ConsumerInfo(stream, "operator-test")
			if err != nil || !reflect.DeepEqual(before.AckFloor, after.AckFloor) || !reflect.DeepEqual(before.Delivered, after.Delivered) || before.Created != after.Created {
				t.Fatalf("consumer changed: %v", err)
			}
			info, err := js.StreamInfo(stream)
			if err != nil || info.State.Msgs != 1 || info.State.Consumers != 1 {
				t.Fatalf("retention or consumers changed: %v", err)
			}
			if err = nc.Flush(); err != nil {
				t.Fatal(err)
			}
			if pending, _, err := sub.Pending(); err != nil || pending != 0 {
				t.Fatalf("task published: %d %v", pending, err)
			}
		})
	}
}

func TestReconcileCommandConnectionErrorRedacted(t *testing.T) {
	cmd := ReconcileCommand()
	cmd.SetArgs([]string{"--data-dir", t.TempDir(), "--task", "task", "--run", "run", "--attempt", "attempt", "--nats-url", "nats://private-user:private-password@127.0.0.1:0"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	err := cmd.ExecuteContext(t.Context())
	if err == nil || strings.Contains(err.Error()+output.String(), "private-") {
		t.Fatalf("connection error leaked credentials: %v %s", err, &output)
	}
}
