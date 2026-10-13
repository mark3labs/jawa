package orchestrator

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttemptWorking(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a    Attempt
		want bool
	}{
		{Attempt{State: "running", RunState: "running"}, true},
		{Attempt{State: "running"}, true},
		{Attempt{State: "running", RunState: "completed"}, false},
		{Attempt{State: "failed", RunState: "running"}, false},
		{Attempt{State: "failed", RunState: "failed", Error: resumePendingNotice}, true},
		{Attempt{State: "running", Error: executionConflictNotice}, false},
		{Attempt{State: "running", Ready: true, Error: resumePendingNotice}, false},
		{Attempt{State: "ready", Error: resumePendingNotice}, false},
	} {
		if got := attemptWorking(tc.a); got != tc.want {
			t.Fatalf("%+v: %v", tc.a, got)
		}
	}
}

func TestPlanningWorking(t *testing.T) {
	t.Parallel()
	for _, session := range []string{"queued", "submitted", "running", "questions", "ready", "approved", "failed", "cancelled", "unknown"} {
		for _, turn := range []string{"queued", "submitted", "running", "waiting", "failed", "cancelled", "completed", "unknown"} {
			d := PlanningDetail{Session: PlanningSession{ID: "current", State: session}, Turns: []PlanningTurn{
				{SessionID: "old", Number: 100, State: "running"},
				{SessionID: "current", Number: 2, State: turn},
				{SessionID: "current", Number: 1, State: "running"},
			}}
			want := workingState(session) && (workingState(turn) || (session == "running" && turn == "waiting"))
			if got := planningWorking(d); got != want {
				t.Errorf("%s/%s: %v", session, turn, got)
			}
		}
	}
	if planningWorking(PlanningDetail{Session: PlanningSession{State: "running"}}) {
		t.Fatal("missing turn counted")
	}
	if !planningWorking(PlanningDetail{Session: PlanningSession{State: "running", Error: resumePendingNotice}, Turns: []PlanningTurn{{State: "failed"}}}) {
		t.Fatal("resume not counted")
	}
}

func TestProjectWorkAndRendering(t *testing.T) {
	t.Parallel()
	s := uiSnapshot{projects: []Project{{ID: "p", Name: "Project"}, {ID: "other", Name: "Other"}}, planning: map[string]PlanningDetail{}}
	for _, status := range []string{"Planning", "Building", "Fix", "Todo", "Done"} {
		s.cards = append(s.cards, Card{ID: status, ProjectID: "p", Status: status, Archived: true})
		s.attempts = append(s.attempts, Attempt{CardID: status, Number: 2, State: "running"}, Attempt{CardID: status, Number: 1, State: "failed"})
	}
	s.cards = append(s.cards, Card{ID: "unrelated", ProjectID: "other", Status: "Building"}, Card{ID: "missing", ProjectID: "p", Status: "Building"})
	s.planning["Planning"] = PlanningDetail{Session: PlanningSession{State: "queued"}, Turns: []PlanningTurn{{State: "submitted"}}}
	want := workSummary{Planning: 1, Building: 1, Fix: 1}
	if got := projectWork(s, "p"); got != want {
		t.Fatalf("got %+v", got)
	}
	if projectWork(s, "other").Total() != 0 {
		t.Fatal("unrelated project counted")
	}
	d := pageData{v: viewParams{View: viewBoard, Project: "p"}, uiSnapshot: s}
	var full, fragments bytes.Buffer
	if err := appPage(d).Render(t.Context(), &full); err != nil {
		t.Fatal(err)
	}
	if err := boardContent(d).Render(t.Context(), &fragments); err != nil {
		t.Fatal(err)
	}
	if err := sidebarNav(d).Render(t.Context(), &fragments); err != nil {
		t.Fatal(err)
	}
	for _, html := range []string{full.String(), fragments.String()} {
		if !strings.Contains(html, `role="group" class="stat" title="`+want.Label()+`" aria-label="`+want.Label()+`"`) ||
			!strings.Contains(html, `aria-label="Project: `+want.Label()+`"`) {
			t.Fatal("full page/fragments disagree on accessible breakdown")
		}
		if !strings.Contains(html, `<strong>3</strong> active`) {
			t.Fatal("missing board total")
		}
	}
}

func TestStoredWorkIndicatorSnapshots(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"Planning", "Fix"} {
		t.Run(mode, func(t *testing.T) {
			a := testApp(t)
			setupTest(t, a)
			w := a.workflow
			s := a.s
			c := workflowCard(t, s)
			var sessionID, taskID string
			if mode == "Planning" {
				if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
					t.Fatal(err)
				}
				detail := planningDetails(t, w, c.ID)
				sessionID, taskID = detail.Session.ID, detail.Turns[0].ID
			} else {
				repairDone(t, w, c)
				if err := w.FixCard(c.ID, "Repair fixture"); err != nil {
					t.Fatal(err)
				}
				a, err := w.CardResult(c.ID)
				if err != nil {
					t.Fatal(err)
				}
				taskID = a.ID
			}
			for _, step := range []struct {
				state, remote string
				active        bool
			}{
				{"queued", "submitted", true}, {"running", "running", true},
				{"questions", "waiting", false}, {"running", "waiting", mode == "Planning"},
				{"running", "running", true}, {"ready", "completed", false}, {"failed", "failed", false},
			} {
				if mode == "Planning" {
					if _, err := s.db.Exec(`UPDATE planning_sessions SET state=? WHERE id=?`, step.state, sessionID); err != nil {
						t.Fatal(err)
					}
					if _, err := s.db.Exec(`UPDATE planning_turns SET state=? WHERE id=?`, step.remote, taskID); err != nil {
						t.Fatal(err)
					}
				} else {
					state := step.state
					if state == "questions" {
						state = "blocked"
					}
					if _, err := s.db.Exec(`UPDATE workflow_attempts SET state=?,run_state=? WHERE id=?`, state, step.remote, taskID); err != nil {
						t.Fatal(err)
					}
				}
				v := viewParams{View: viewBoard, Project: c.ProjectID}
				r := httptest.NewRequest("GET", "/snapshot", nil)
				patch, err := Snapshot(r, s, w, v)
				if err != nil {
					t.Fatal(err)
				}
				d, err := loadPageData(t.Context(), s, w, "", v)
				if err != nil {
					t.Fatal(err)
				}
				var full bytes.Buffer
				if err := appPage(d).Render(t.Context(), &full); err != nil {
					t.Fatal(err)
				}
				total := 0
				if step.active {
					total = 1
				}
				for _, html := range []string{patch, full.String()} {
					if !strings.Contains(html, fmt.Sprintf("<strong>%d</strong> active", total)) {
						t.Fatalf("%s: wrong active count", step.state)
					}
					label := `nav-count live" title="` + projectWork(d.uiSnapshot, c.ProjectID).Label() + `"`
					if strings.Contains(html, label) != step.active {
						t.Fatalf("%s: wrong project badge", step.state)
					}
				}
				if !strings.Contains(patch, `id="sidebar-nav"`) || strings.Contains(patch, `id="card-dialog"`) {
					t.Fatal("wrong patch roots")
				}
			}
		})
	}
}
