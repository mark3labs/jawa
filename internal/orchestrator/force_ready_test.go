package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
	"time"
)

func readyFixture(t *testing.T) (*Workflow, Card, Attempt, string) {
	t.Helper()
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	a := actionAttempt(t, w, c, "failed", "failed")
	c.Status = "Building"
	_, p, _, _, err := readySnapshot(w.s.db, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) { return strings.Repeat("a", 40), nil })
	return w, c, a, forceReadyToken(c, a, p)
}

func TestForceReadyGates(t *testing.T) {
	for _, state := range []string{"queued", "submitted", "running", "waiting", "interrupted", "failed", "blocked", "abandoned"} {
		for _, lane := range []string{"Todo", "Planning", "Building", "Fix", "Done"} {
			t.Run(state+lane, func(t *testing.T) {
				c := Card{ID: "c", Status: lane}
				a := Attempt{ID: "a", CardID: "c", State: state, RunState: state}
				all := []Attempt{a}
				want := (lane == "Building" || lane == "Fix") && (state == "failed" || state == "blocked" || state == "abandoned")
				if got := canAction(c, all, "force-ready"); got != want {
					t.Fatalf("got %v want %v", got, want)
				}
				d := pageData{attempts: all, pendingResumes: map[string]bool{}}
				if d.canForceReady(c) != want {
					t.Fatal("presentation disagrees")
				}
				d.pendingResumes[c.ID] = true
				if d.canForceReady(c) {
					t.Fatal("pending resume offered")
				}
			})
		}
	}
	c := Card{ID: "c", Status: "Building"}
	for _, a := range []Attempt{{CardID: "c", Ready: true}, {CardID: "c", State: "ready"}, {CardID: "c", Error: resumePendingNotice}, {CardID: "c", Error: executionConflictNotice}} {
		if canAction(c, []Attempt{a}, "force-ready") {
			t.Fatal("ineligible", a)
		}
	}
	if canAction(c, nil, "force-ready") {
		t.Fatal("legacy")
	}
	if canAction(c, []Attempt{{CardID: "c", Number: 1, State: "running"}, {CardID: "c", Number: 2, State: "failed"}}, "force-ready") {
		t.Fatal("older active")
	}
}

func TestForceReadyRegressionAndReplay(t *testing.T) {
	w, c, before, token := readyFixture(t)
	mustExec(t, w, `INSERT INTO auto_resumes(task_id,card_id,planning,failure_token,due_at,next_check_at,state) VALUES(?,?,0,'token',1,1,'scheduled')`, before.TaskID, c.ID)
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "operator inspected", "admin", true); err != nil {
		t.Fatal(err)
	}
	after, err := w.CardResult(c.ID)
	if err != nil || !after.Ready || after.State != "ready" || after.PRURL != "https://github.com/example/repo/pull/55" || after.RunState != before.RunState || after.OutcomeJSON != before.OutcomeJSON || after.Result != before.Result {
		t.Fatalf("%+v %v", after, err)
	}
	assertActionLane(t, w, c.ProjectID, "Done", c.ID)
	audit, err := w.s.readyOverrides()
	if err != nil || len(audit) != 1 || audit[before.ID].Operator != "admin" || audit[before.ID].Reason != "operator inspected" || audit[before.ID].HeadSHA != strings.Repeat("a", 40) {
		t.Fatalf("%+v %v", audit, err)
	}
	var prior string
	if err := w.s.db.QueryRow(`SELECT prior_snapshot FROM workflow_ready_overrides`).Scan(&prior); err != nil {
		t.Fatal(err)
	}
	var original Attempt
	if json.Unmarshal([]byte(prior), &original) != nil || !reflect.DeepEqual(before, original) {
		t.Fatal("prior snapshot changed")
	}
	var count int
	for _, table := range []string{"auto_resumes", "typed_reports"} {
		if err := w.s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatal(table, count, err)
		}
	}
	w.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) {
		t.Error("duplicate made provider call")
		return "", errors.New("unavailable")
	})
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "changed reason", "other", true); err != nil {
		t.Fatal(err)
	}
	again, _ := w.s.readyOverrides()
	if !reflect.DeepEqual(audit, again) {
		t.Fatal("replay changed audit")
	}
	if err := w.ForceReady(t.Context(), c.ID, "stale", "55", "", "admin", true); err == nil {
		t.Fatal("different token accepted")
	}
	if err := w.MarkPRMerged(t.Context(), c.ID, after.ID, after.PRURL); err != nil {
		t.Fatal(err)
	}
	if err := w.FixCard(c.ID, "repair"); err != nil {
		t.Fatal(err)
	}
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true); err == nil {
		t.Fatal("repair reused token")
	}
	history, _ := w.s.readyOverrides()
	if !reflect.DeepEqual(audit, history) {
		t.Fatal("historical provenance lost")
	}
}

func TestForceReadyRejectsWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"ack", "operator", "empty", "zero", "negative", "text", "overflow", "reason", "utf8", "stale", "unavailable", "provider", "pending", "rollback"} {
		t.Run(scenario, func(t *testing.T) {
			w, c, before, token := readyFixture(t)
			number, reason, operator, ack := "55", "", "admin", true
			switch scenario {
			case "ack":
				ack = false
			case "operator":
				operator = ""
			case "empty":
				number = ""
			case "zero":
				number = "0"
			case "negative":
				number = "-1"
			case "text":
				number = "https://github.com/example/repo/pull/55"
			case "overflow":
				number = strings.Repeat("9", 30)
			case "reason":
				reason = strings.Repeat("é", 501)
			case "utf8":
				reason = string([]byte{255})
			case "stale":
				token = "wrong"
			case "unavailable":
				w.SetProviderIdentity(nil)
			case "provider":
				w.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) { return "", errors.New("no") })
			case "pending":
				mustExec(t, w, `INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('r',?,?,0,'{}','{}',1)`, before.TaskID, c.ID)
			case "rollback":
				mustExec(t, w, `CREATE TRIGGER reject_done BEFORE UPDATE OF status ON cards WHEN NEW.status='Done' BEGIN SELECT RAISE(ABORT,'test rollback'); END`)
			}
			if err := w.ForceReady(t.Context(), c.ID, token, number, reason, operator, ack); err == nil {
				t.Fatal("accepted")
			}
			after, _ := w.CardResult(c.ID)
			audit, _ := w.s.readyOverrides()
			if !reflect.DeepEqual(before, after) || len(audit) != 0 {
				t.Fatal("rejection mutated evidence")
			}
			assertActionLane(t, w, c.ProjectID, "Building", c.ID)
		})
	}
}

func TestForceReadyStaleDuringIdentity(t *testing.T) {
	for _, query := range []string{
		`UPDATE workflow_attempts SET event_seq=event_seq+1`,
		`UPDATE workflow_attempts SET outcome_json='changed'`,
		`UPDATE workflow_attempts SET error='changed'`,
		`UPDATE workflow_attempts SET state='blocked'`,
		`UPDATE workflow_attempts SET run_state='cancelled'`,
		`UPDATE workflow_attempts SET pr_url='https://github.com/example/repo/pull/56'`,
		`UPDATE workflow_attempts SET repair_pr_url='https://github.com/example/repo/pull/55'`,
		`UPDATE projects SET base_branch='other'`,
		`UPDATE cards SET status='Todo'`,
	} {
		t.Run(query, func(t *testing.T) {
			w, c, _, token := readyFixture(t)
			w.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) {
				mustExec(t, w, query)
				return strings.Repeat("a", 40), nil
			})
			if err := w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true); err == nil {
				t.Fatal("stale accepted")
			}
			audit, _ := w.s.readyOverrides()
			if len(audit) != 0 {
				t.Fatal("stale audit")
			}
		})
	}
}

func TestForceReadyConcurrent(t *testing.T) {
	w, c, _, token := readyFixture(t)
	var wg sync.WaitGroup
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	w.SetProviderIdentity(func(ctx context.Context, _ Attempt, _ string) (string, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return strings.Repeat("a", 40), nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { results <- w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true) })
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("preflight deadlock")
		}
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	audit, _ := w.s.readyOverrides()
	if len(audit) != 1 {
		t.Fatal("duplicate audit")
	}
}

func TestProviderIdentity(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, scenario := range []string{"open", "draft", "conflict", "merged", "closed", "state", "number", "repo", "base", "branch", "sha", "http", "redirect", "malformed", "oversize", "cancel", "token", "task"} {
			t.Run(provider+scenario, func(t *testing.T) {
				t.Setenv("JAWA_GITHUB_TOKEN", "test")
				t.Setenv("GITHUB_TOKEN", "")
				t.Setenv("JAWA_FORGEJO_TOKEN", "test")
				w := actionWorkflow(t)
				repo := "https://github.com/example/repo"
				if provider == "forgejo" {
					repo = "https://codeberg.org/example/repo"
				}
				p, err := w.s.CreateProjectDetails("identity", provider, repo, "main")
				if err != nil {
					t.Fatal(err)
				}
				c, err := w.s.CreateCard(p.ID, "test", "test")
				if err != nil {
					t.Fatal(err)
				}
				a := actionAttempt(t, w, c, "failed", "failed")
				branch, _ := assignedBranch(a)
				prURL, _ := projectPRURL(provider, repo, 55)
				pr := providerPR{Number: 55, State: "open"}
				pr.Head.Ref, pr.Head.SHA, pr.Head.Repo.FullName = branch, strings.Repeat("a", 40), "example/repo"
				pr.Base.Ref, pr.Base.Repo.FullName = "main", "example/repo"
				switch scenario {
				case "draft":
					pr.Draft = true
				case "conflict":
					no := false
					pr.Mergeable = &no
				case "merged":
					pr.State = "closed"
					pr.Merged = true
				case "closed":
					pr.State = "closed"
				case "state":
					pr.State = "unknown"
				case "number":
					pr.Number = 56
				case "repo":
					pr.Head.Repo.FullName = "other/repo"
				case "base":
					pr.Base.Ref = "other"
				case "branch":
					pr.Head.Ref = "other"
				case "sha":
					pr.Head.SHA = "bad"
				case "task":
					a.TaskJSON = "{}"
				case "token":
					t.Setenv("JAWA_GITHUB_TOKEN", "")
					t.Setenv("GITHUB_TOKEN", "")
					t.Setenv("JAWA_FORGEJO_TOKEN", "")
					t.Setenv("FORGEJO_TOKEN", "")
				}
				calls := 0
				client := &http.Client{Transport: providerTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.Method != "GET" || r.URL.Path != "/repos/example/repo/pulls/55" && r.URL.Path != "/api/v1/repos/example/repo/pulls/55" {
						t.Error("unexpected endpoint", r.Method, r.URL.Path)
					}
					data, _ := json.Marshal(pr)
					code := 200
					switch scenario {
					case "http":
						code = 500
					case "redirect":
						code = 302
					case "malformed":
						data = []byte("{")
					case "oversize":
						data = []byte(strings.Repeat(" ", 5<<20))
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Location": {"https://evil.example"}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
				})}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if scenario == "cancel" {
					cancel()
				}
				check := ProviderIdentity(w.s, client)
				sha, err := check(ctx, a, prURL)
				want := scenario == "open" || scenario == "draft" || scenario == "conflict" || scenario == "merged"
				if (err == nil) != want || want && sha != pr.Head.SHA {
					t.Fatalf("sha=%s err=%v", sha, err)
				}
				if calls > 1 {
					t.Fatal("readiness endpoints contacted")
				}
				if want {
					w.SetProviderIdentity(check)
					c.Status = "Building"
					token := forceReadyToken(c, a, p)
					if err := w.ForceReady(ctx, c.ID, token, "55", "", "admin", true); err != nil {
						t.Fatal("real provider single connection", err)
					}
				}
			})
		}
	}
}

func TestForceReadyHTTPAndProvenance(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	if _, err := a.s.db.Exec(`INSERT INTO admin(id,username,password_hash) VALUES(1,'operator','unused')`); err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, a.s)
	before := actionAttempt(t, a.workflow, c, "failed", "failed")
	c.Status = "Building"
	_, p, _, _, _ := readySnapshot(a.s.db, c.ID)
	token := forceReadyToken(c, before, p)
	a.workflow.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) { return strings.Repeat("a", 40), nil })
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "expected": {token}, "pr_number": {"55"}, "reason": {"<script>alert(1)</script>"}, "operator": {"attacker"}}
	body := assertSSE(t, sseRequest(a, "POST", "/cards/force-ready", form, session, csrf))
	if !strings.Contains(body, "confirm that CI") || !strings.Contains(body, `id="ready-error"`) || strings.Contains(body, "datastar-patch-signals") {
		t.Fatal(body)
	}
	form.Set("acknowledge", "yes")
	body = assertSSE(t, sseRequest(a, "POST", "/cards/force-ready", form, session, csrf))
	for _, text := range []string{"Card moved to Done. Readiness checks were bypassed; the PR was not merged.", "Manually readied"} {
		if !strings.Contains(body, text) {
			t.Fatal("missing", text)
		}
	}
	audit, _ := a.s.readyOverrides()
	if audit[before.ID].Operator != "operator" {
		t.Fatal("browser attribution")
	}
	d, err := loadPageData(t.Context(), a.s, a.workflow, csrf.Value, viewParams{View: viewPlanning, Card: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := planningContent(d).Render(t.Context(), &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "<script>") || !strings.Contains(output.String(), "&lt;script&gt;") {
		t.Fatal("unescaped reason")
	}
	for _, component := range []func() error{
		func() error { return boardContent(d).Render(t.Context(), &output) },
		func() error { return runsContent(d).Render(t.Context(), &output) },
		func() error { return archiveContent(d).Render(t.Context(), &output) },
	} {
		if err := component(); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(output.String(), audit[before.ID].MarkedAt.Format("2006-01-02 15:04:05.000 UTC")) {
		t.Fatal("missing timestamp")
	}
}

func TestForceReadyPRSelection(t *testing.T) {
	p := Project{Provider: "github", Repo: "https://github.com/example/repo"}
	c := Card{Status: "Building"}
	pr := "https://github.com/example/repo/pull/55"
	for _, tc := range []struct {
		a      Attempt
		number string
		lane   string
		ok     bool
	}{
		{Attempt{}, "55", "Building", true},
		{Attempt{PRURL: pr}, "", "Building", true},
		{Attempt{PRURL: pr}, "55", "Building", true},
		{Attempt{PRURL: pr}, "56", "Building", false},
		{Attempt{PRURL: pr}, "0", "Building", false},
		{Attempt{PRURL: pr + "?x=y"}, "", "Building", false},
		{Attempt{PRURL: pr, RepairPRURL: pr + "1"}, "", "Fix", false},
		{Attempt{PRURL: pr}, "", "Fix", false},
		{Attempt{RepairPRURL: pr}, "", "Fix", true},
	} {
		c.Status = tc.lane
		got, err := forceReadyPR(c, tc.a, p, tc.number)
		if (err == nil) != tc.ok || tc.ok && got != pr {
			t.Fatalf("%+v: %s %v", tc, got, err)
		}
	}
	w, card, _, token := readyFixture(t)
	if err := w.ForceReady(t.Context(), card.ID, token, "55", strings.Repeat("é", 500), "admin", true); err != nil {
		t.Fatal("1000-byte boundary", err)
	}
}

func TestForceReadyStoppedAndRepairOnly(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprint(repair), func(t *testing.T) {
			w, c, a, _ := readyFixture(t)
			if repair {
				mustExec(t, w, `UPDATE cards SET status='Fix' WHERE id=?`, c.ID)
				mustExec(t, w, `UPDATE workflow_attempts SET repair_pr_url='https://github.com/example/repo/pull/55' WHERE id=?`, a.ID)
			} else {
				mustExec(t, w, `UPDATE workflow_attempts SET state='running',run_state='running' WHERE id=?`, a.ID)
				if err := w.ForceStop(c.ID); err != nil {
					t.Fatal(err)
				}
			}
			c, p, all, _, err := readySnapshot(w.s.db, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			token := forceReadyToken(c, latestAttempt(c.ID, all)[0], p)
			if err := w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestForceReadyFencesLateDeliveries(t *testing.T) {
	w, c, a, token := readyFixture(t)
	w.observeReport = fakeReportObserver
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true); err != nil {
		t.Fatal(err)
	}
	before, _ := w.CardResult(c.ID)
	audit, _ := w.s.readyOverrides()
	ev := client.StatusEvent{Version: 1, TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", EventID: "late", Type: "run_state", Seq: 2, State: runtime.RunRunning}
	if err := w.event(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"agent", "other"} {
		if err := w.result(t.Context(), client.Outcome{Version: 1, TaskID: a.TaskID, AgentID: agent, RunID: "run", AttemptID: "attempt", State: runtime.RunCompleted, Response: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []reportprotocol.Kind{reportprotocol.CompleteTask, reportprotocol.ReportBlocker} {
		payload := reportprotocol.Payload{PRNumber: 55}
		if kind == reportprotocol.ReportBlocker {
			payload = reportprotocol.Payload{Reason: "late"}
		}
		request, err := reportprotocol.NewRequest(reportprotocol.Identity{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", TurnID: "turn", ToolCallID: string(kind)}, kind, payload)
		if err != nil {
			t.Fatal(err)
		}
		if ack, err := w.acceptReport(t.Context(), request); err == nil || ack.Accepted {
			t.Fatal("late typed report accepted")
		}
	}
	if err := w.applyStatus(a, client.Status{TaskID: a.TaskID, AgentID: "agent", RunID: "run", AttemptID: "attempt", State: runtime.RunRunning}); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(a.TaskID, "run", "attempt"); err == nil {
		t.Fatal("late reconciliation accepted")
	}
	after, _ := w.CardResult(c.ID)
	later, _ := w.s.readyOverrides()
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(audit, later) {
		t.Fatal("late delivery mutated ready authority")
	}
	assertActionLane(t, w, c.ProjectID, "Done", c.ID)
}

func TestForceReadyConflictPolling(t *testing.T) {
	w, c, _, token := readyFixture(t)
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "", "admin", true); err != nil {
		t.Fatal(err)
	}
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{State: "conflict", HeadSHA: strings.Repeat("a", 40)}, nil
	})
	if err := w.PollPRConflicts(t.Context()); err != nil {
		t.Fatal(err)
	}
	check, err := w.CardPRCheck(c.ID)
	if err != nil || check.State != "conflict" {
		t.Fatal(check, err)
	}
	if _, err := w.RefreshPRChecks(t.Context(), c.ProjectID, c.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := w.s.db.QueryRow(`SELECT count(*) FROM typed_reports`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestForceReadyPersistenceAndViews(t *testing.T) {
	w, c, original, token := readyFixture(t)
	if err := w.ForceReady(t.Context(), c.ID, token, "55", "inspection", "admin", true); err != nil {
		t.Fatal(err)
	}
	check := func(w *Workflow, archived bool) {
		t.Helper()
		if err := w.s.SetCardArchived(c.ID, archived); err != nil {
			t.Fatal(err)
		}
		d, err := readyTestPage(t, w, viewParams{View: viewBoard, Project: c.ProjectID, State: "all", Card: c.ID})
		if err != nil {
			t.Fatal(err)
		}
		audit := d.readyOverrides[original.ID]
		if audit.Operator != "admin" {
			t.Fatal("audit lost")
		}
		var out bytes.Buffer
		if archived {
			if err := archiveContent(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := boardContent(d).Render(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
		}
		if !strings.Contains(out.String(), "Manually readied") || !strings.Contains(out.String(), audit.MarkedAt.Format("2006-01-02 15:04:05.000 UTC")) {
			t.Fatal("board/archive provenance missing")
		}
		out.Reset()
		if err := runsContent(d).Render(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "Manually readied") || !strings.Contains(out.String(), "Ready (manual)") {
			t.Fatal("runs provenance missing")
		}
	}
	check(w, false)
	check(w, true)
	check(w, false)
	// Open the same persistent DB via a new Store/Workflow, as after restart.
	var path string
	if err := w.s.db.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	again := &Workflow{s: reopened, ctx: t.Context()}
	check(again, false)
	if err := again.MarkPRMerged(t.Context(), c.ID, original.ID, "https://github.com/example/repo/pull/55"); err != nil {
		t.Fatal(err)
	}
	d, err := readyTestPage(t, again, viewParams{View: viewBoard, Project: c.ProjectID})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := boardContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ">Merged<") || !strings.Contains(out.String(), "Manually readied") {
		t.Fatal("merged hid provenance")
	}
	if err := again.FixCard(c.ID, "repair"); err != nil {
		t.Fatal(err)
	}
	latest, _ := again.CardResult(c.ID)
	audit, _ := reopened.readyOverrides()
	if _, ok := audit[latest.ID]; ok {
		t.Fatal("new attempt inherited provenance")
	}
	d, err = readyTestPage(t, again, viewParams{View: viewRuns, Card: c.ID, State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runsContent(d).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "Manually readied") != 2 { // badge text and tooltip
		t.Fatal("historical run provenance", out.String())
	}
}

func TestForceReadyHTTPNativeAuthCSRF(t *testing.T) {
	a := testApp(t)
	session, csrf := sseCookies(t, a)
	if _, err := a.s.db.Exec(`INSERT INTO admin(id,username,password_hash) VALUES(1,'admin','unused')`); err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, a.s)
	attempt := actionAttempt(t, a.workflow, c, "failed", "failed")
	c.Status = "Building"
	_, p, _, _, _ := readySnapshot(a.s.db, c.ID)
	form := url.Values{"card_id": {c.ID}, "expected": {forceReadyToken(c, attempt, p)}, "pr_number": {"55"}, "acknowledge": {"yes"}, "_csrf": {csrf.Value}}
	a.workflow.SetProviderIdentity(func(context.Context, Attempt, string) (string, error) { return strings.Repeat("a", 40), nil })
	request := func(cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/cards/force-ready", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		a.ServeHTTP(response, r)
		return response
	}
	if response := request(); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/login" {
		t.Fatal("unauthenticated action", response.Code)
	}
	form.Set("_csrf", "wrong")
	if response := request(session, csrf); response.Code != http.StatusForbidden {
		t.Fatal("CSRF accepted", response.Code)
	}
	form.Set("_csrf", csrf.Value)
	if response := request(session, csrf); response.Code != http.StatusSeeOther {
		t.Fatal("native action", response.Code, response.Body.String())
	}
	ready, _ := a.workflow.CardResult(c.ID)
	if !ready.Ready {
		t.Fatal("native action did not commit")
	}
	if _, err := a.s.db.Exec(`UPDATE sessions SET expires_at=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.requestOperator(httptest.NewRequest("GET", "/", nil)); err == nil {
		t.Fatal("missing session accepted")
	}
	if response := request(session, csrf); response.Header().Get("Location") != "/login" {
		t.Fatal("expired session accepted")
	}
}

func TestForceReadyContextCancellation(t *testing.T) {
	for _, source := range []string{"request", "workflow"} {
		t.Run(source, func(t *testing.T) {
			w, c, _, token := readyFixture(t)
			request, stopRequest := context.WithCancel(t.Context())
			defer stopRequest()
			workflow, stopWorkflow := context.WithCancel(t.Context())
			defer stopWorkflow()
			w.ctx = workflow
			w.SetProviderIdentity(func(ctx context.Context, _ Attempt, _ string) (string, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("missing deadline")
				}
				if source == "request" {
					stopRequest()
				} else {
					stopWorkflow()
				}
				<-ctx.Done()
				return "", ctx.Err()
			})
			if err := w.ForceReady(request, c.ID, token, "55", "", "admin", true); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			audit, _ := w.s.readyOverrides()
			if len(audit) != 0 {
				t.Fatal("cancelled action wrote audit")
			}
		})
	}
}

func readyTestPage(t *testing.T, w *Workflow, v viewParams) (pageData, error) {
	t.Helper()
	d, err := loadPageData(t.Context(), w.s, nil, "csrf", v)
	if err != nil {
		return d, err
	}
	d.attempts, err = w.Attempts()
	if err == nil {
		d.runs, err = w.s.runHistory(t.Context(), d.attempts)
	}
	if err != nil {
		return d, err
	}
	for _, c := range d.cards {
		if check, e := w.CardPRCheck(c.ID); e == nil {
			d.prChecks[c.ID] = check
		}
	}
	return d, nil
}
