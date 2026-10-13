package orchestrator

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	bonnieclient "github.com/mark3labs/bonnie/client"
	client "github.com/mark3labs/bonnie/client/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAPIKeyPersistsAndIsPrivate(t *testing.T) {
	a := testApp(t)
	key, err := a.s.apiSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	again, err := a.s.apiSigningKey()
	if err != nil || !bytes.Equal(key, again) {
		t.Fatal("key not retained", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatal("wrong key size")
	}
	overrides, err := a.s.DashboardOverrides()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := overrides["api_signing_key"]; ok {
		t.Fatal("private key exposed")
	}
	if _, err = a.s.db.Exec(`UPDATE settings SET value='bad' WHERE key='api_signing_key'`); err != nil {
		t.Fatal(err)
	}
	if _, err = a.s.apiSigningKey(); err == nil {
		t.Fatal("malformed key accepted")
	}
}
func pinnedDetailAttempt(t *testing.T, a *app) Attempt {
	t.Helper()
	card := workflowCard(t, a.s)
	if err := a.workflow.MoveCard(card.ID, "Building", 0); err != nil {
		t.Fatal(err)
	}
	attempt, err := a.workflow.CardResult(card.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.s.db.Exec(`UPDATE workflow_attempts SET agent_id='detail-agent',run_id='detail-run',remote_attempt_id='remote-1',run_state='running',state='running' WHERE id=?`, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = a.workflow.attemptByID(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}
func TestRunDetailRoutesAndReadOnly(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	attempt := pinnedDetailAttempt(t, a)
	path := detailPath(attempt, "chat")
	status(t, request(a, "GET", path, nil), http.StatusSeeOther)
	status(t, request(a, "GET", "/runs/"+attempt.ID+"/events", nil), http.StatusUnauthorized)
	page := request(a, "GET", path, nil, session, csrf)
	status(t, page, 200)
	for _, text := range []string{"run-detail-content", "run-detail-controls", "Agent unreachable", "/runs/" + attempt.ID + "/events?tab=chat"} {
		if !strings.Contains(page.Body.String(), text) {
			t.Fatal("missing", text)
		}
	}
	if strings.Contains(page.Body.String(), "@get('/events?view=runs") {
		t.Fatal("generic stream opened")
	}
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable")
	}
	status(t, request(a, "GET", "/runs/abc", nil, session), 404)
	status(t, request(a, "GET", "/runs/"+attempt.ID+"/chat", nil, session), 405)
	status(t, request(a, "POST", "/runs/"+attempt.ID+"/chat", url.Values{"text": {"hello"}}, session), 403)
	status(t, request(a, "POST", "/runs/"+attempt.ID+"/chat", url.Values{"text": {"hello"}, "_csrf": {csrf.Value}}, session, csrf), 409)
}
func TestRunPolicyDoesNotResumeUnreportedCompletion(t *testing.T) {
	a := testApp(t)
	attempt := pinnedDetailAttempt(t, a)
	snap := &bonnieclient.Snapshot{RunID: attempt.RunID, TurnID: "turn-1", State: runtime.RunRunning}
	if p := a.workflow.runPolicy(attempt, snap, true); !p.Send || p.Mode != "steer" || !p.Stop {
		t.Fatal(p)
	}
	for _, state := range []runtime.RunState{runtime.RunWaiting, runtime.RunFailed, runtime.RunCancelled, runtime.RunRetired, runtime.RunCompleted} {
		snap.State = state
		if p := a.workflow.runPolicy(attempt, snap, true); p.Send {
			t.Fatal("enabled", state, p)
		}
	}
	attempt.Ready = true
	snap.State = runtime.RunCompleted
	if p := a.workflow.runPolicy(attempt, snap, true); !p.Send || p.Mode != "queue" {
		t.Fatal(p)
	}
	if p := a.workflow.runPolicy(attempt, snap, false); p.Send {
		t.Fatal("offline enabled")
	}
	if err := a.workflow.cancelAudit(attempt, "turn-1", "uncertain", ""); err != nil {
		t.Fatal(err)
	}
	if p := a.workflow.runPolicy(attempt, snap, true); p.Send {
		t.Fatal("pending cancellation enabled")
	}
}
func TestRunDisplayEscapesUntrustedContent(t *testing.T) {
	messages := projectRunMessages([]kit.LLMMessage{{Role: "assistant", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "<script>alert(1)</script>\n\n**bold** ![](https://tracker.invalid/x)"}}}, {Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "<img src=x onerror=alert(1)>"}}}})
	var out bytes.Buffer
	if err := runDetailContent(runDetail{Messages: messages}).Render(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	html := out.String()
	if strings.Contains(html, "<script>") || strings.Contains(html, "<img") {
		t.Fatal("unsafe HTML")
	}
	if !strings.Contains(html, "<strong>bold</strong>") || !strings.Contains(html, "&lt;img") {
		t.Fatal("wrong projection")
	}
}
func TestWorkerDirectACLAfterReload(t *testing.T) {
	a := testApp(t)
	setupTest(t, a)
	for _, credentials := range [][2]string{{"worker", "long-agent-password"}, {"rotated-worker", "rotated-agent-password"}} {
		if credentials[0] != "worker" {
			if err := a.rotateNATS(a.ns.ClientURL(), credentials[0], credentials[1]); err != nil {
				t.Fatal(err)
			}
		}
		errors := make(chan error, 10)
		nc, err := nats.Connect(a.ns.ClientURL(), nats.UserInfo(credentials[0], credentials[1]), nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, e error) { errors <- e }))
		if err != nil {
			t.Fatal(err)
		}
		victim, err := a.workflow.resumeConn.SubscribeSync("bonnie.api.agent.victim")
		if err != nil {
			t.Fatal(err)
		}
		if err = a.workflow.resumeConn.Flush(); err != nil {
			t.Fatal(err)
		}
		if err = nc.Publish("bonnie.api.agent.victim", []byte("fixture")); err != nil {
			t.Fatal(err)
		}
		if _, err = nc.SubscribeSync("_INBOX.bonnieapi.fixture"); err != nil {
			t.Fatal(err)
		}
		if err = nc.Flush(); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			select {
			case <-errors:
			case <-time.After(time.Second):
				t.Fatal("missing asynchronous denial")
			}
		}
		if _, err = victim.NextMsg(50 * time.Millisecond); err == nil {
			t.Fatal("denied API message delivered")
		}
		js, _ := nc.JetStream(nats.MaxWait(100 * time.Millisecond))
		kv, err := js.KeyValue("jawa_control")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = kv.Put("api_public_key", []byte("replacement")); err == nil {
			t.Fatal("key overwrite accepted")
		}
		if _, err = js.AddStream(&nats.StreamConfig{Name: "capture", Subjects: []string{"bonnie.api.>"}}); err == nil {
			t.Fatal("capture stream allowed")
		}
		_ = victim.Unsubscribe()
		nc.Close()
	}
}

func TestRunHubCapacityAndEqualSequenceActivity(t *testing.T) {
	a := testApp(t)
	pool := a.workflow.runAPI
	var releases []func()
	for i := 0; i < 16; i++ {
		_, release, err := pool.observe(Attempt{AgentID: "capacity-agent", RunID: fmt.Sprintf("run-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err := pool.observe(Attempt{AgentID: "capacity-agent", RunID: "overflow"}); err == nil {
		t.Fatal("stream capacity not bounded")
	}
	for _, release := range releases {
		release()
	}
	var events []runtime.Event
	for i := 0; i < 300; i++ {
		events, _ = boundedEvents(events, runtime.Event{Seq: 5, Type: "live", Text: fmt.Sprint(i)})
	}
	if len(events) != 256 || events[0].Text != "44" || events[255].Text != "299" {
		t.Fatal("equal-sequence live activity lost or unbounded")
	}
}

func TestFollowupCannotChangeTerminalWorkflow(t *testing.T) {
	a := testApp(t)
	attempt := pinnedDetailAttempt(t, a)
	_, err := a.s.db.Exec(`UPDATE workflow_attempts SET ready=1,state='ready',run_state='completed',pr_url='https://github.com/example/repo/pull/1',outcome_json='{"terminal":true}' WHERE id=?`, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.s.db.Exec(`UPDATE cards SET status='Done' WHERE id=?`, attempt.CardID); err != nil {
		t.Fatal(err)
	}
	before, _ := a.workflow.attemptByID(attempt.ID)
	event := client.StatusEvent{Version: 1, EventID: "followup-event", Type: "run_state", Seq: 999, State: "running", Target: client.Target{TaskID: before.TaskID, AgentID: before.AgentID, RunID: before.RunID, AttemptID: before.RemoteAttemptID}}
	if err = a.workflow.event(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	after, _ := a.workflow.attemptByID(attempt.ID)
	if after.Ready != before.Ready || after.RunState != before.RunState || after.PRURL != before.PRURL || after.AgentID != before.AgentID || after.OutcomeJSON != before.OutcomeJSON {
		t.Fatal("follow-up changed workflow", after)
	}
	var lane string
	if err = a.s.db.QueryRow(`SELECT status FROM cards WHERE id=?`, before.CardID).Scan(&lane); err != nil || lane != "Done" {
		t.Fatal(lane, err)
	}
}

func TestObserveWaitsForTabSwitchTeardown(t *testing.T) {
	a := testApp(t)
	p := a.workflow.runAPI
	attempt := Attempt{AgentID: "tab-switch-agent", RunID: "tab-switch-run"}
	old := &runHub{done: make(chan struct{}), viewers: 0, cancel: func() {}}
	p.mu.Lock()
	p.hubs[runKey(attempt)] = old
	p.mu.Unlock()
	returned := make(chan *runHub, 1)
	go func() {
		h, release, err := p.observe(attempt)
		if err != nil {
			returned <- nil
			return
		}
		defer release()
		returned <- h
	}()
	select {
	case <-returned:
		t.Fatal("returned before old upstream closed")
	case <-time.After(20 * time.Millisecond):
	}
	p.mu.Lock()
	delete(p.hubs, runKey(attempt))
	close(old.done)
	p.mu.Unlock()
	select {
	case h := <-returned:
		if h == nil || h == old {
			t.Fatal("tab switch did not get replacement")
		}
	case <-time.After(time.Second):
		t.Fatal("tab switch did not resume")
	}
}
