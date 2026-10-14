package orchestrator

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func planningHistoryFixture(t *testing.T, a *app) RunRecord {
	t.Helper()
	c := workflowCard(t, a.s)
	if err := a.workflow.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	runs, err := a.s.runHistory(t.Context(), nil)
	if err != nil || len(runs) != 1 {
		t.Fatal(runs, err)
	}
	return runs[0]
}
func TestPlanningRunStates(t *testing.T) {
	for _, tc := range []struct {
		state, session, interaction, kind, err, label, filter string
		pending                                               bool
	}{
		{state: "queued", label: "Queued", filter: "active"},
		{state: "submitted", label: "Investigating", filter: "active"},
		{state: "running", label: "Investigating", filter: "active"},
		{state: "failed", pending: true, label: "Resuming", filter: "active"},
		{state: "running", err: resumePendingNotice, label: "Resuming", filter: "active"},
		{state: "waiting", interaction: "pending", kind: "clarify", label: "Needs input", filter: "blocked"},
		{state: "waiting", interaction: "pending", kind: "propose_spec", label: "Awaiting review", filter: "blocked"},
		{state: "waiting", interaction: "pending", kind: "report_blocker", label: "Needs input", filter: "blocked"},
		{state: "waiting", interaction: "queued", label: "Resuming", filter: "active"},
		{state: "waiting", session: "approved", interaction: "answered", label: "Resuming", filter: "active"},
		{state: "completed", session: "approved", label: "Completed", filter: "all"},
		{state: "failed", label: "Failed", filter: "failed"},
		{state: "cancelled", interaction: "queued", label: "Cancelled", filter: "all"},
		{state: "waiting", session: "approved", label: "Approved", filter: "all"},
	} {
		t.Run(tc.label+tc.state+tc.kind, func(t *testing.T) {
			r := RunRecord{Mode: "Planning", State: tc.state, SessionState: tc.session, InteractionStatus: tc.interaction, InteractionKind: tc.kind, Error: tc.err, PendingResume: tc.pending}
			if r.label() != tc.label || !r.matches(tc.filter) || r.matches("ready") {
				t.Fatal(r.label(), tc)
			}
			if tc.filter != "active" && r.matches("active") {
				t.Fatal("paused/terminal active", tc)
			}
		})
	}
}
func TestPlanningHistoryRetentionAndRendering(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	first := planningHistoryFixture(t, a)
	cards, err := a.s.Cards("")
	if err != nil {
		t.Fatal(err)
	}
	card, _ := cardByID(cards, first.CardID)
	board := request(a, "GET", "/board?project="+card.ProjectID, nil, session, csrf)
	if !strings.Contains(board.Body.String(), `href="`+runsForCardPath(card.ID)+`"`) || !strings.Contains(board.Body.String(), "View runs") {
		t.Fatal("planning navigation missing")
	}
	check := func(want int) []RunRecord {
		t.Helper()
		attempts, err := a.workflow.Attempts()
		if err != nil {
			t.Fatal(err)
		}
		runs, err := a.s.runHistory(t.Context(), attempts)
		if err != nil || len(runs) != want {
			t.Fatal(runs, err)
		}
		if len(filterHistory(runs, viewParams{Card: "other", State: "all"})) != 0 {
			t.Fatal("card filter")
		}
		page := request(a, "GET", runsForCardPath(card.ID), nil, session, csrf)
		status(t, page, 200)
		snapshot := request(a, "GET", "/snapshot?view=runs&card="+card.ID, nil, session, csrf)
		status(t, snapshot, 200)
		for _, response := range []string{page.Body.String(), snapshot.Body.String()} {
			if !strings.Contains(response, "run-planning-"+first.ID) {
				t.Fatal("planning row missing")
			}
		}
		return runs
	}
	check(1)
	unpinned := request(a, "GET", runsForCardPath(card.ID), nil, session, csrf)
	if strings.Contains(unpinned.Body.String(), first.path("chat")) {
		t.Fatal("unpinned open")
	}
	// A new task within one session adds history, rather than replacing the task.
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET state='failed' WHERE id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.Exec(`UPDATE planning_sessions SET state='failed' WHERE card_id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.workflow.RetryPlanning(card.ID); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := a.workflow.CancelPlanning(card.ID); err != nil {
		t.Fatal(err)
	}
	check(2)
	if err := a.workflow.MoveCard(card.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	runs := check(3)
	if runs[0].ID == first.ID {
		t.Fatal("not newest-first")
	}
	if err := a.s.SetCardArchived(card.ID, true); err != nil {
		t.Fatal(err)
	}
	archive := request(a, "GET", "/archive", nil, session, csrf)
	if !strings.Contains(archive.Body.String(), runsForCardPath(card.ID)) {
		t.Fatal("archive navigation")
	}
	check(3)
	// Approval queues a separate implementation execution; planning stays distinct.
	if _, err := a.s.db.Exec(`UPDATE planning_sessions SET state='approved' WHERE card_id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.s.db.Exec(`UPDATE cards SET status='Todo' WHERE id=?`, card.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.workflow.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	runs = check(4)
	if countHistory(runs, "ready") != 0 {
		t.Fatal("plan increments PR readiness")
	}
	// Deterministic equal timestamp ordering and no duplicate resume rows.
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET created_at=1,agent_id='history-agent',run_id='history-run',remote_attempt_id='remote'`); err != nil {
		t.Fatal(err)
	}
	one := check(4)
	two := check(4)
	for i := range one {
		if one[i].ID != two[i].ID {
			t.Fatal("unstable order")
		}
	}
}
func TestPlanningRunDetailRoutesAndDeliveries(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	r := planningHistoryFixture(t, a)
	status(t, request(a, "GET", r.path("chat"), nil, session), 404)
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET agent_id='history-agent',run_id='history-run',remote_attempt_id='remote' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err := a.workflow.historyByID(t.Context(), "Planning", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	status(t, request(a, "GET", r.path("chat"), nil), 303)
	status(t, request(a, "GET", r.basePath()+"/events", nil), 401)
	for _, tab := range []string{"chat", "trace"} {
		page := request(a, "GET", r.path(tab), nil, session, csrf)
		status(t, page, 200)
		html := page.Body.String()
		for _, want := range []string{r.basePath() + "/events?tab=" + tab} {
			if !strings.Contains(html, want) {
				t.Fatal("missing", want)
			}
		}
		for _, absent := range []string{`id="run-composer"`, "Send message", "Stop current turn", "Copy branch", "Agent unreachable", "Planning is observation-only"} {
			if strings.Contains(html, absent) {
				t.Fatal("mutation visible", absent)
			}
		}
		if page.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("cacheable")
		}
	}
	for _, action := range []string{"chat", "stop"} {
		status(t, request(a, "POST", r.basePath()+"/"+action, url.Values{"_csrf": {csrf.Value}, "text": {"hello"}}, session, csrf), http.StatusForbidden)
	}
	for _, path := range []string{"/runs/" + r.ID, "/runs/planning/" + uuid.NewString(), r.basePath() + "/events/extra", "/runs/planning/invalid"} {
		status(t, request(a, "GET", path, nil, session), 404)
	}
	for _, tuple := range []string{"", `"agent_id":"other",`, `"run_id":"other",`, `"attempt_id":"other",`} {
		if _, err := a.s.db.Exec(`INSERT INTO planning_deliveries(turn_id,kind,payload,received_at) VALUES(?,'event',?,1)`, r.ID, "{"+tuple+`"state":"running"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.s.db.Exec(`INSERT INTO planning_deliveries(turn_id,kind,payload,received_at) VALUES(?,'event','malformed',1)`, r.ID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	d := a.historyDetailModel(&http.Request{URL: &url.URL{}, Form: url.Values{"tab": {"trace"}}}, r, runObservation{Unreachable: true}, false)
	if err := runDetailContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Planning event") {
		t.Fatal("local delivery rendered as API trace")
	}
	if _, err := a.s.db.Exec(`UPDATE planning_turns SET remote_attempt_id='bad/destination' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	status(t, request(a, "GET", r.path("chat"), nil, session), 404)
}

func TestPlanningDetailStreamIdentityFence(t *testing.T) {
	for _, end := range []string{"identity", "delete", "revoke"} {
		t.Run(end, func(t *testing.T) {
			a := testApp(t)
			session, csrf := setupTest(t, a)
			record := planningHistoryFixture(t, a)
			if _, err := a.s.db.Exec(`UPDATE planning_turns SET agent_id='stream-agent',run_id='stream-run',remote_attempt_id='remote' WHERE id=?`, record.ID); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(a)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", server.URL+record.basePath()+"/events?tab=trace", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.AddCookie(session)
			req.AddCookie(csrf)
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != 200 || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatal(response.Status, response.Header)
			}
			reader := bufio.NewReader(response.Body)
			var frame strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				frame.WriteString(line)
				if line == "\n" {
					break
				}
			}
			if !strings.Contains(frame.String(), "run-detail-content") || !strings.Contains(frame.String(), "run-detail-controls") {
				t.Fatal(frame.String())
			}
			switch end {
			case "identity":
				_, err = a.s.db.Exec(`UPDATE planning_turns SET run_id='changed' WHERE id=?`, record.ID)
			case "delete":
				_, err = a.s.db.Exec(`DELETE FROM planning_turns WHERE id=?`, record.ID)
			case "revoke":
				_, err = a.s.db.Exec(`DELETE FROM sessions`)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = io.Copy(io.Discard, reader); err != nil {
				t.Fatal("stream not gracefully fenced", err)
			}
		})
	}
}

func TestMissingRunLookupReturnsZeroRecord(t *testing.T) {
	a := testApp(t)
	for _, mode := range []string{"Planning", "Build"} {
		r, err := a.workflow.historyByID(t.Context(), mode, uuid.NewString())
		if err == nil || r.Mode != "" || r.ID != "" || !r.CreatedAt.IsZero() {
			t.Fatal(mode, r, err)
		}
	}
}
