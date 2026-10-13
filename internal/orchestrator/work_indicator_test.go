package orchestrator

import (
	"bytes"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAttemptWorking(t *testing.T) {
	for _, state := range []string{"queued", "submitted", "running", "ready", "failed", "blocked", "waiting", "interrupted", "cancelled", "completed", "", "unknown"} {
		for _, remote := range []string{"", "queued", "submitted", "running", "waiting", "interrupted", "failed", "cancelled", "completed", "unknown"} {
			t.Run(state+"/"+remote, func(t *testing.T) {
				want := workQueuedOrRunning(state) && (remote == "" || workQueuedOrRunning(remote))
				if got := attemptWorking(Attempt{State: state, RunState: remote}); got != want {
					t.Fatalf("got %v, want %v", got, want)
				}
			})
		}
	}
	for _, tc := range []struct {
		name string
		a    Attempt
		want bool
	}{
		{"resume failed remote", Attempt{State: "running", RunState: "failed", Error: resumePendingNotice}, true},
		{"ready overrides resume", Attempt{State: "running", Ready: true, Error: resumePendingNotice}, false},
		{"reconciliation", Attempt{State: "running", RunState: "running", Error: executionConflictNotice}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if attemptWorking(tc.a) != tc.want {
				t.Fatal(tc.a)
			}
		})
	}
}

func TestPlanningWorking(t *testing.T) {
	for _, session := range []string{"queued", "submitted", "running", "questions", "ready", "approved", "failed", "cancelled", "", "unknown"} {
		for _, turn := range []string{"queued", "submitted", "running", "waiting", "failed", "cancelled", "completed", "", "unknown"} {
			t.Run(session+"/"+turn, func(t *testing.T) {
				d := PlanningDetail{Session: PlanningSession{ID: "current", State: session}, Turns: []PlanningTurn{{SessionID: "old", Number: 99, State: "running"}, {SessionID: "current", Number: 2, State: turn}, {SessionID: "current", Number: 1, State: "running"}}}
				want := workQueuedOrRunning(session) && (workQueuedOrRunning(turn) || (session == "running" && turn == "waiting"))
				if got := planningWorking(d); got != want {
					t.Fatalf("got %v, want %v", got, want)
				}
			})
		}
	}
	if planningWorking(PlanningDetail{Session: PlanningSession{State: "running"}}) {
		t.Fatal("missing turn counted")
	}
	if !planningWorking(PlanningDetail{Session: PlanningSession{State: "running", Error: resumePendingNotice}}) {
		t.Fatal("pending resume not counted")
	}
}

func TestProjectWork(t *testing.T) {
	snap := uiSnapshot{
		cards:    []Card{{ID: "p", ProjectID: "a", Status: "Planning", Archived: true}, {ID: "b", ProjectID: "a", Status: "Building"}, {ID: "f", ProjectID: "a", Status: "Fix"}, {ID: "historical", ProjectID: "a", Status: "Fix"}, {ID: "missing", ProjectID: "a", Status: "Planning"}, {ID: "missing-attempt", ProjectID: "a", Status: "Fix"}, {ID: "todo", ProjectID: "a", Status: "Todo"}, {ID: "done", ProjectID: "a", Status: "Done"}, {ID: "other", ProjectID: "other", Status: "Building"}},
		planning: map[string]PlanningDetail{"p": {Session: PlanningSession{State: "queued"}, Turns: []PlanningTurn{{State: "submitted"}}}},
		attempts: []Attempt{{CardID: "b", Number: 2, State: "running"}, {CardID: "b", Number: 1, State: "failed"}, {CardID: "f", State: "queued"}, {CardID: "historical", Number: 3, State: "ready", Ready: true}, {CardID: "historical", Number: 1, State: "running"}, {CardID: "todo", State: "running"}, {CardID: "done", State: "running"}, {CardID: "other", State: "running"}},
	}
	got := projectWork(snap, "a")
	if got != (workSummary{Planning: 1, Building: 1, Fix: 1}) {
		t.Fatal(got)
	}
	if got.Label() != "3 active work items: 1 planning, 1 building, 1 fixing (includes queued and archived work)" {
		t.Fatal(got.Label())
	}
	if !strings.Contains((workSummary{Fix: 1}).Label(), "1 active work item:") {
		t.Fatal("singular label")
	}
}

// Exercise the real snapshot loader and patch roots through disposable durable state.
func TestWorkIndicatorSnapshots(t *testing.T) {
	store, nc := workflowFixture(t)
	if err := migrateWorkflow(store); err != nil {
		t.Fatal(err)
	}
	presence, err := presencenats.New(t.Context(), nc, presencenats.Config{Bucket: "work_fixture", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	wf := &Workflow{s: store, presence: presence, ctx: t.Context()}
	p, err := wf.s.CreateProjectDetails("Work <project>", "github", "https://github.com/example/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	c, err := wf.s.CreateCardDetails(p.ID, "Work", "", "")
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := wf.s.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE cards SET status='Planning' WHERE id=?`, c.ID)
	exec(`INSERT INTO planning_sessions(id,card_id,repo,base_branch,provider,title,description,issue_url,created_at,updated_at) VALUES('session',?,'example/repo','main','github','Work','','',0,0)`, c.ID)
	exec(`INSERT INTO planning_turns(id,session_id,number,task_json,created_at,updated_at) VALUES('turn','session',1,'{}',0,0)`)
	v := viewParams{View: viewBoard, Project: p.ID}
	check := func(active bool) {
		t.Helper()
		r := httptest.NewRequest("GET", boardPath(p.ID), nil)
		patch, err := Snapshot(r, wf.s, wf, v)
		if err != nil {
			t.Fatal(err)
		}
		d, err := loadPageData(t.Context(), wf.s, wf, "", v)
		if err != nil {
			t.Fatal(err)
		}
		var page bytes.Buffer
		if err := appPage(d).Render(t.Context(), &page); err != nil {
			t.Fatal(err)
		}
		n := "0"
		if active {
			n = "1"
		}
		for _, html := range []string{patch, page.String()} {
			if !strings.Contains(html, `<strong>`+n+`</strong> active`) {
				t.Fatal("board total missing", html)
			}
			if strings.Contains(html, `class="nav-count live" title=`) != active {
				t.Fatal("sidebar activity mismatch")
			}
			if !strings.Contains(html, `aria-label="`+projectWork(d.uiSnapshot, p.ID).Label()+`"`) {
				t.Fatal("accessible breakdown missing")
			}
		}
		for _, root := range []string{`id="sidebar-nav"`, `id="board-content"`} {
			if !strings.Contains(patch, root) {
				t.Fatal("missing patch root", root)
			}
		}
		for _, forbidden := range []string{`id="card-dialog"`, `id="project-dialog"`, `<html`} {
			if strings.Contains(patch, forbidden) {
				t.Fatal("draft/shell patched", forbidden)
			}
		}
	}
	for _, step := range []struct {
		session, turn, notice string
		active                bool
	}{
		{"queued", "queued", "", true}, {"queued", "submitted", "", true}, {"running", "running", "", true}, {"questions", "waiting", "", false}, {"running", "waiting", "", true}, {"running", "running", "", true}, {"ready", "waiting", "", false}, {"running", "failed", resumePendingNotice, true}, {"running", "running", "", true}, {"failed", "failed", "", false},
	} {
		exec(`UPDATE planning_sessions SET state=?,error=? WHERE id='session'`, step.session, step.notice)
		exec(`UPDATE planning_turns SET state=? WHERE id='turn'`, step.turn)
		check(step.active)
	}
	exec(`UPDATE cards SET status='Fix',archived=1 WHERE id=?`, c.ID)
	exec(`INSERT INTO workflow_attempts(id,card_id,number,task_id,task_json,created_at,updated_at) VALUES('attempt',?,1,'task','{}',0,0)`, c.ID)
	for _, step := range []struct {
		state, remote, notice string
		ready, active         bool
	}{
		{"queued", "", "", false, true}, {"submitted", "", "", false, true}, {"running", "running", "", false, true}, {"ready", "completed", "", true, false}, {"failed", "failed", "", false, false}, {"running", "failed", resumePendingNotice, false, true}, {"running", "running", "", false, true}, {"blocked", "interrupted", "", false, false},
	} {
		exec(`UPDATE workflow_attempts SET state=?,run_state=?,error=?,ready=? WHERE id='attempt'`, step.state, step.remote, step.notice, step.ready)
		check(step.active)
	}
}
