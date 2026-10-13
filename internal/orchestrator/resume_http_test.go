package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestFailedResumeWorkLaneHTTP(t *testing.T) {
	t.Parallel()
	for _, lane := range []string{"Building", "Fix", "Planning"} {
		t.Run(lane, func(t *testing.T) {
			app := testApp(t)
			session, csrf := setupTest(t, app)
			w := app.workflow
			if err := w.s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
				t.Fatal(err)
			}
			card := workflowCard(t, w.s)
			if lane == "Fix" {
				repairDone(t, w, card)
				if err := w.FixCard(card.ID, "Resolve conflict"); err != nil {
					t.Fatal(err)
				}
			} else if err := w.MoveCard(card.ID, lane, 0); err != nil {
				t.Fatal(err)
			}
			var before Attempt
			route := "/cards/resume"
			if lane == "Planning" {
				route = "/cards/planning/resume"
				d := planningDetails(t, w, card.ID)
				before.TaskID = d.Turns[0].ID
			} else {
				var err error
				before, err = w.CardResult(card.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			outcome := client.Outcome{Version: 1, TaskID: before.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote", State: runtime.RunFailed, Error: "connection reset"}
			if err := w.result(t.Context(), outcome); err != nil {
				t.Fatal(err)
			}
			target := client.Target{TaskID: before.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}
			sub, err := w.resumeConn.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
				raw, _ := json.Marshal(client.Status{Version: 1, Target: target, State: runtime.RunFailed, TurnID: "turn", Seq: 20})
				_ = m.Respond(raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			if err := w.resumeConn.Flush(); err != nil {
				t.Fatal(err)
			}
			token := ""
			if lane == "Planning" {
				token = planningDetails(t, w, card.ID).ResumeToken
			} else {
				before, _ = w.CardResult(card.ID)
				token = resumeToken(before)
			}
			if token == "" {
				t.Fatal("missing token")
			}
			form := url.Values{"_csrf": {csrf.Value}, "view": {"board"}, "card_id": {card.ID}}
			for _, bad := range []string{"", "stale"} {
				form.Set("expected_resume", bad)
				status(t, request(app, "POST", route, form, session, csrf), http.StatusBadRequest)
				var count int
				if err := w.s.db.QueryRow(`SELECT count(*) FROM failed_resumes WHERE card_id=?`, card.ID).Scan(&count); err != nil || count != 0 {
					t.Fatal("invalid token reserved work", count, err)
				}
			}
			form.Set("expected_resume", token)
			status(t, request(app, "POST", route, form, session, csrf), http.StatusSeeOther)
			status(t, request(app, "POST", route, form, session, csrf), http.StatusBadRequest)
			if lane != "Planning" {
				for name, action := range map[string]func(string) error{"retry": w.Retry, "reset": w.ResetCard, "delete": w.DeleteCard} {
					if err := action(card.ID); err == nil {
						t.Fatal("pending action allowed", name)
					}
				}
				after, err := w.CardResult(card.ID)
				if err != nil || after.ID != before.ID || after.Number != before.Number || after.TaskID != before.TaskID || after.RunID != before.RunID || after.RemoteAttemptID != before.RemoteAttemptID || after.TaskJSON != before.TaskJSON || after.Branch != before.Branch || after.RepairMessage != before.RepairMessage || after.RepairPRURL != before.RepairPRURL || after.RepairSourceNumber != before.RepairSourceNumber {
					t.Fatal("assignment changed", after, err)
				}
			}
			assertActionLane(t, w, card.ProjectID, lane, card.ID)
		})
	}
}
