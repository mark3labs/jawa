package orchestrator

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
)

func TestExplicitUnblockHTTPAndRendering(t *testing.T) {
	for _, pr := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "force-ready"}[pr], func(t *testing.T) {
			app := testApp(t)
			session, csrf := setupTest(t, app)
			if err := app.s.SetDashboardOverrides(map[string]string{"auto-resume-failed-runs": "false"}); err != nil {
				t.Fatal(err)
			}
			card := workflowCard(t, app.s)
			if err := app.workflow.MoveCard(card.ID, "Building", 0); err != nil {
				t.Fatal(err)
			}
			a, _ := app.workflow.CardResult(card.ID)
			prURL := ""
			route := "/cards/unblock"
			label := "Unblock and resume…"
			if pr {
				prURL = "https://github.com/example/repo/pull/7"
				route = "/cards/force-ready"
				label = "Force ready…"
			}
			if _, err := app.s.db.Exec(`UPDATE workflow_attempts SET state='blocked',run_state='completed',agent_id='agent',run_id='run',remote_attempt_id='remote',error='<script>blocker</script>',pr_url=? WHERE id=?`, prURL, a.ID); err != nil {
				t.Fatal(err)
			}
			a, _ = app.workflow.CardResult(card.ID)
			sub, err := app.workflow.resumeConn.Subscribe("bonnie.queries.agent", func(m *nats.Msg) {
				raw, _ := json.Marshal(client.Status{Version: 1, Target: client.Target{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "remote"}, State: runtime.RunCompleted, TurnID: "turn", Seq: 20})
				_ = m.Respond(raw)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sub.Unsubscribe() }()
			_ = app.workflow.resumeConn.Flush()
			body := request(app, "GET", boardPath(card.ProjectID), nil, session, csrf).Body.String()
			if !strings.Contains(body, label) || !strings.Contains(body, `id="unblock-dialog"`) || strings.Contains(body, "<script>blocker</script>") {
				t.Fatal("missing action/dialog or unsafe blocker", body)
			}
			status(t, request(app, "GET", route, nil, session, csrf), http.StatusMethodNotAllowed)
			status(t, request(app, "POST", route, nil, session, csrf), http.StatusForbidden)
			status(t, request(app, "POST", route, nil), http.StatusSeeOther)
			form := url.Values{"_csrf": {csrf.Value}, "card_id": {card.ID}, "view": {"board"}, "project": {card.ProjectID}, "prompt": {"finish"}, "acknowledged": {"true"}}
			status(t, request(app, "POST", route, form, session, csrf), http.StatusBadRequest)
			form.Set("expected_unblock", unblockToken(a, "Building"))
			if pr {
				form.Del("acknowledged")
			} else {
				form.Set("prompt", " ")
			}
			failed := assertSSE(t, sseRequest(app, "POST", route, form, session, csrf))
			if strings.Contains(failed, `"unblockOpen":false`) {
				t.Fatal("failure discarded draft")
			}
			form.Set("prompt", "finish")
			form.Set("acknowledged", "true")
			body = assertSSE(t, sseRequest(app, "POST", route, form, session, csrf))
			if !strings.Contains(body, `"unblockOpen":false`) || !strings.Contains(body, `"unblockPrompt":""`) {
				t.Fatal("success did not reset dialog", body)
			}
			if pr && !strings.Contains(body, "Ready (manual override)") {
				t.Fatal("override hidden", body)
			}
		})
	}
}
