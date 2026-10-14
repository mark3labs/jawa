package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAutoMergePolicy(t *testing.T) {
	for _, global := range []bool{false, true} {
		for _, mode := range []string{"inherit", "on", "off", "invalid"} {
			want := mode == "on" || mode == "inherit" && global
			if autoMergeEffective(global, mode) != want {
				t.Fatalf("%v/%s", global, mode)
			}
		}
	}
	for n := 0; n < 20; n++ {
		d := mergeBackoff(n)
		if d < time.Minute || d > 30*time.Minute {
			t.Fatal(d)
		}
	}
}

func mergeTestAdapter(t *testing.T, s *Store, a Attempt, provider, scenario string, writes *int) *mergeAdapter {
	t.Helper()
	branch, err := assignedBranch(a)
	if err != nil {
		t.Fatal(err)
	}
	merged := false
	transport := providerTransport(func(r *http.Request) (*http.Response, error) {
		host, auth := "api.github.com", "Bearer test-token"
		if provider == "forgejo" {
			host, auth = "forge.example", "token test-token"
		}
		if r.URL.Host != host || r.Header.Get("Authorization") != auth {
			t.Errorf("untrusted target or auth")
		}
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		body := ""
		code := 200
		switch {
		case strings.HasSuffix(path, "/merge"):
			*writes++
			wantMethod := "PUT"
			if provider == "forgejo" {
				wantMethod = "POST"
			}
			if r.Method != wantMethod {
				t.Error(r.Method)
			}
			var payload map[string]any
			if json.NewDecoder(r.Body).Decode(&payload) != nil {
				t.Error("payload")
			}
			sha := strings.Repeat("a", 40)
			if provider == "forgejo" {
				if payload["Do"] != "squash" || payload["head_commit_id"] != sha || payload["force_merge"] != false || payload["merge_when_checks_succeed"] != false || payload["delete_branch_after_merge"] != false {
					t.Errorf("unsafe payload: %v", payload)
				}
			} else if payload["sha"] != sha || payload["merge_method"] != "squash" {
				t.Error(payload)
			}
			if scenario == "reject" {
				code = 405
				break
			}
			if scenario == "conflict" {
				code = 409
				break
			}
			merged = true
			if scenario == "lost" {
				return nil, fmt.Errorf("lost response with secret")
			}
			body = `{"merged":true,"sha":"` + sha + `"}`
			if provider == "forgejo" {
				body = ""
			}
		case path == "/repos/acme/repo/pulls/7":
			state := "open"
			if merged {
				state = "closed"
			}
			body = fmt.Sprintf(`{"number":7,"state":%q,"merged":%v,"mergeable":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"acme/repo"}},"base":{"ref":"main","sha":%q,"repo":{"full_name":"acme/repo"}}}`, state, merged, branch, strings.Repeat("a", 40), strings.Repeat("b", 40))
		case path == "/repos/acme/repo/branches/main":
			if provider == "forgejo" {
				body = `{"name":"main","protected":true,"enable_status_check":true,"status_check_contexts":["ci"]}`
			} else {
				body = `{"protected":false}`
			}
		case strings.HasSuffix(path, "/protection/required_status_checks"):
			body = `{"contexts":[],"checks":[]}`
		case strings.HasSuffix(path, "/rules/branches/main"):
			body = `[]`
		case strings.HasSuffix(path, "/check-runs"):
			body = `{"total_count":1,"check_runs":[{"head_sha":"` + strings.Repeat("a", 40) + `","name":"ci","status":"completed","conclusion":"success","app":{"id":1}}]}`
		case strings.HasSuffix(path, "/status"):
			if provider == "forgejo" {
				body = `{"sha":"` + strings.Repeat("a", 40) + `","state":"success","total_count":1,"statuses":[{"context":"ci","status":"success"}]}`
			} else {
				body = `{"sha":"` + strings.Repeat("a", 40) + `","state":"success","total_count":0,"statuses":[]}`
			}
		case strings.HasSuffix(path, "/reviews"):
			body = `[]`
		case path == "/graphql":
			body = `{"data":{"repository":{"pullRequest":{"headRefOid":"` + strings.Repeat("a", 40) + `","reviewThreads":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}}}`
		default:
			t.Errorf("unexpected endpoint %s", path)
		}
		return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return newMergeAdapter(s, &http.Client{Transport: transport})
}

func TestAutoMergeDispatchAndReplay(t *testing.T) {
	for _, provider := range []string{"github", "forgejo"} {
		for _, scenario := range []string{"success", "lost", "reject", "conflict"} {
			t.Run(provider+"/"+scenario, func(t *testing.T) {
				s, a := conflictFixture(t, provider)
				persistConflictAttempt(t, s, a)
				w := &Workflow{s: s, ctx: context.Background()}
				if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
					t.Fatal(err)
				}
				writes := 0
				adapter := mergeTestAdapter(t, s, a, provider, scenario, &writes)
				if _, err := adapter.prepare(t.Context(), a); err != nil {
					t.Fatalf("prepare: %v", err)
				}
				w.merges = &autoMergeWorker{adapter: adapter}
				w.autoMergePass(t.Context())
				w.autoMergePass(t.Context())
				if writes != 1 {
					var category string
					_ = s.db.QueryRow(`SELECT category FROM auto_merge_operations`).Scan(&category)
					t.Fatalf("writes=%d category=%s", writes, category)
				}
				var state string
				if err := s.db.QueryRow(`SELECT state FROM auto_merge_operations WHERE head_sha<>''`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				want := "merged"
				if scenario == "reject" {
					want = "rejected"
				}
				if scenario == "conflict" {
					want = "deferred"
				}
				if state != want {
					t.Fatalf("state=%s want %s", state, want)
				}
				current, err := w.CardResult(a.CardID)
				if err != nil || !current.Ready {
					t.Fatal("readiness lost", err)
				}
				if scenario == "success" {
					if err = w.DeleteCard(a.CardID); err != nil {
						t.Fatal(err)
					}
					var count int
					if err = s.db.QueryRow(`SELECT count(*) FROM auto_merge_operations`).Scan(&count); err != nil || count != 1 {
						t.Fatal("tombstone lost", err, count)
					}
				}
			})
		}
	}
}

func TestAutoMergeExclusions(t *testing.T) {
	for _, scenario := range []string{"default", "off", "archived", "force", "manual", "pending", "active", "invalid-global"} {
		t.Run(scenario, func(t *testing.T) {
			s, a := conflictFixture(t, "github")
			persistConflictAttempt(t, s, a)
			w := &Workflow{s: s, ctx: context.Background()}
			if scenario != "default" && scenario != "invalid-global" {
				if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			switch scenario {
			case "off":
				err = w.SetCardAutoMerge(a.CardID, "on", "off")
			case "archived":
				err = s.SetCardArchived(a.CardID, true)
			case "force":
				_, err = s.db.Exec(`INSERT INTO workflow_ready_overrides VALUES(?,?,'sha','token','{}','admin','',0)`, a.ID, a.PRURL)
			case "manual":
				_, err = s.db.Exec(`INSERT INTO workflow_pr_merge_overrides VALUES(?,?,'github','https://github.com/acme/repo.git','main',0)`, a.ID, a.PRURL)
			case "pending":
				_, err = s.db.Exec(`INSERT INTO failed_resumes(id,task_id,card_id,planning,request_json,previous_outcome,created_at) VALUES('r',?,?,0,'{}','{}',0)`, a.TaskID, a.CardID)
			case "active":
				_, err = s.db.Exec(`UPDATE workflow_attempts SET run_state='running'`)
			case "invalid-global":
				_, err = s.db.Exec(`INSERT INTO settings VALUES('dashboard:auto-merge','bad')`)
			}
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			w.merges = &autoMergeWorker{adapter: mergeTestAdapter(t, s, a, "github", "success", &writes)}
			w.autoMergePass(t.Context())
			if writes != 0 {
				t.Fatal("excluded write")
			}
		})
	}
}

func TestAutoMergePreferenceAndReservation(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: context.Background()}
	if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetCardAutoMerge(a.CardID, "inherit", "off"); err == nil {
		t.Fatal("stale accepted")
	}
	token, provider, repo, base, err := mergeConfig(s.db, a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO auto_merge_operations VALUES('op',?,?,?,?,?,?,?,?,'reserved','',0,0,0,0)`, a.CardID, a.ID, provider, repo, base, a.PRURL, strings.Repeat("a", 40), token)
	if err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]func() error{"reset": func() error { return w.ResetCard(a.CardID) }, "delete": func() error { return w.DeleteCard(a.CardID) }, "archive": func() error { return s.SetCardArchived(a.CardID, true) }, "fix": func() error { return w.FixCard(a.CardID, "repair") }} {
		if action() == nil {
			t.Fatal(name, "passed reservation")
		}
	}
	if err = w.SetCardAutoMerge(a.CardID, "on", "off"); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = s.db.QueryRow(`SELECT state FROM auto_merge_operations`).Scan(&state)
	if state != "deferred" {
		t.Fatal(state)
	}
}

func TestAutoMergeIssueDraft(t *testing.T) {
	s, a := conflictFixture(t, "github")
	var p Project
	if err := s.db.QueryRow(`SELECT id,provider,repo FROM projects`).Scan(&p.ID, &p.Provider, &p.Repo); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	_, fresh, err := s.reserveIssueSubmissionOptions(token, p, "issue", "body", "on")
	if err != nil || !fresh {
		t.Fatal(err)
	}
	if _, _, err = s.reserveIssueSubmissionOptions(token, p, "issue", "body", "off"); err == nil {
		t.Fatal("mismatched preference accepted")
	}
	if err = s.recordIssueOutcome(token, "uncertain", projectIssue{}); err != nil {
		t.Fatal(err)
	}
	if err = s.finalizeIssueSubmission(token, "", true); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err = s.db.QueryRow(`SELECT auto_merge_mode FROM cards WHERE id<>?`, a.CardID).Scan(&mode); err != nil || mode != "on" {
		t.Fatal(mode, err)
	}
}

func TestAutoMergeBrowserFixture(t *testing.T) {
	path := os.Getenv("JAWA_AUTO_MERGE_HTML")
	if path == "" {
		t.Skip("fixture export only")
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/style.css"><script defer src="/app.js"></script></head><body class="style-nova"><main class="p-6 max-w-xl"><h1>Auto merge preferences</h1><p>Global default Off. Explicit On overrides it.</p><form id="preference"><input type="hidden" name="_csrf" value="fixture">`)
	if err := autoMergeControls("fixture", "inherit").Render(t.Context(), &b); err != nil {
		t.Fatal(err)
	}
	b.WriteString(`<button type="submit">Save</button><button type="reset">Reset</button></form></main></body></html>`)
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMergeReconcileWithoutResend(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: context.Background()}
	if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
		t.Fatal(err)
	}
	token, provider, repo, base, err := mergeConfig(s.db, a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO auto_merge_operations VALUES('op',?,?,?,?,?,?,?,?,'uncertain','interrupted',0,0,0,0)`, a.CardID, a.ID, provider, repo, base, a.PRURL, strings.Repeat("a", 40), token)
	if err != nil {
		t.Fatal(err)
	}
	writes := 0
	w.merges = &autoMergeWorker{adapter: mergeTestAdapter(t, s, a, "github", "success", &writes)}
	w.autoMergePass(t.Context())
	w.autoMergePass(t.Context())
	if writes != 0 {
		t.Fatal("uncertainty blindly resent")
	}
	var state string
	_ = s.db.QueryRow(`SELECT state FROM auto_merge_operations`).Scan(&state)
	if state != "deferred" {
		t.Fatal(state)
	}
}

func TestAutoMergePolicyRaceDuringPreparation(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: context.Background()}
	if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
		t.Fatal(err)
	}
	writes := 0
	adapter := mergeTestAdapter(t, s, a, "github", "success", &writes)
	original := adapter.client.Transport
	changed := false
	adapter.client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
		if !changed {
			changed = true
			if err := w.SetCardAutoMerge(a.CardID, "on", "off"); err != nil {
				t.Fatal(err)
			}
		}
		return original.RoundTrip(r)
	})
	w.merges = &autoMergeWorker{adapter: adapter}
	w.autoMergePass(t.Context())
	if writes != 0 {
		t.Fatal("stale policy dispatched")
	}
}

func TestAutoMergeGlobalExistingReadyAndReset(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: context.Background()}
	writes := 0
	w.merges = &autoMergeWorker{adapter: mergeTestAdapter(t, s, a, "github", "success", &writes)}
	w.autoMergePass(t.Context())
	if writes != 0 {
		t.Fatal("absent setting enabled")
	}
	if err := s.SetDashboardOverrides(map[string]string{"auto-merge": "true"}); err != nil {
		t.Fatal(err)
	}
	w.autoMergePass(t.Context())
	if writes != 1 {
		t.Fatal("existing ready not merged")
	}
	if err := s.ClearDashboardOverrides(); err != nil {
		t.Fatal(err)
	}
	values, err := s.DashboardOverrides()
	if err != nil || values["auto-merge"] != "" {
		t.Fatal(values, err)
	}
}

func TestAutoMergeHTTP(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	c := workflowCard(t, a.s)
	form := url.Values{"_csrf": {csrf.Value}, "card_id": {c.ID}, "expected_mode": {"inherit"}, "auto_merge_mode": {"on"}, "view": {"board"}}
	status(t, request(a, "POST", "/cards/auto-merge", form, session, csrf), 303)
	status(t, request(a, "POST", "/cards/auto-merge", form, session, csrf), 400)
	form.Set("expected_mode", "on")
	form.Set("auto_merge_mode", "off")
	body := assertSSE(t, sseRequest(a, "POST", "/cards/auto-merge", form, session, csrf))
	if !strings.Contains(body, "datastar-patch-signals") {
		t.Fatal("missing finite success")
	}
	form.Set("auto_merge_mode", "invalid")
	status(t, request(a, "POST", "/cards/auto-merge", form, session, csrf), 400)
	form.Del("_csrf")
	status(t, request(a, "POST", "/cards/auto-merge", form, session, csrf), 403)
}
func TestAutoMergeRestartLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProject("project")
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CreateCardOptions(p.ID, "card", "", "", "on")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`INSERT INTO auto_merge_operations VALUES('op',?,'attempt','github','repository','main','url','sha','config','inflight','',0,0,0,0)`, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var state, mode string
	if err = s.db.QueryRow(`SELECT state FROM auto_merge_operations`).Scan(&state); err != nil || state != "uncertain" {
		t.Fatal(state, err)
	}
	if err = s.db.QueryRow(`SELECT auto_merge_mode FROM cards`).Scan(&mode); err != nil || mode != "on" {
		t.Fatal(mode, err)
	}
	if _, err = s.db.Exec(`DELETE FROM cards`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM auto_merge_operations`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestAutoMergeDisableDuringDispatch(t *testing.T) {
	s, a := conflictFixture(t, "github")
	persistConflictAttempt(t, s, a)
	w := &Workflow{s: s, ctx: context.Background()}
	if err := w.SetCardAutoMerge(a.CardID, "inherit", "on"); err != nil {
		t.Fatal(err)
	}
	writes := 0
	adapter := mergeTestAdapter(t, s, a, "github", "success", &writes)
	original := adapter.client.Transport
	adapter.client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/merge") {
			if err := w.SetCardAutoMerge(a.CardID, "on", "off"); err != nil {
				t.Fatal(err)
			}
		}
		return original.RoundTrip(r)
	})
	w.merges = &autoMergeWorker{adapter: adapter}
	w.autoMergePass(t.Context())
	var state string
	if err := s.db.QueryRow(`SELECT state FROM auto_merge_operations WHERE head_sha<>''`).Scan(&state); err != nil || state != "merged" || writes != 1 {
		t.Fatal("merge mislabeled after disable", state, writes, err)
	}
}

func TestAutoMergeWriteBoundaryFailures(t *testing.T) {
	for _, scenario := range []string{"redirect", "oversized", "malformed", "rate", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			s, a := conflictFixture(t, "github")
			writes := 0
			adapter := mergeTestAdapter(t, s, a, "github", "success", &writes)
			v, err := adapter.prepare(t.Context(), a)
			if err != nil {
				t.Fatal(err)
			}
			original := adapter.client.Transport
			calls := 0
			adapter.client.Transport = providerTransport(func(r *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(r.URL.Path, "/merge") {
					return original.RoundTrip(r)
				}
				calls++
				if scenario == "cancel" {
					return nil, context.Canceled
				}
				code, body := 200, "{"
				headers := make(http.Header)
				switch scenario {
				case "redirect":
					code = 302
					headers.Set("Location", "https://evil.example/")
				case "oversized":
					body = strings.Repeat("x", (4<<20)+1)
				case "rate":
					code = 429
					headers.Set("Retry-After", "3600")
				}
				return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: headers, Request: r}, nil
			})
			out := adapter.dispatch(t.Context(), a, v)
			if calls != 1 || out.state == "merged" || out.state != "deferred" {
				t.Fatal(calls, out)
			}
			if scenario == "rate" && time.Until(out.retryAfter) < 59*time.Minute {
				t.Fatal("retry-after lost")
			}
		})
	}
}
