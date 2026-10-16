package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func mcpTestServer(t *testing.T) (*Workflow, string, string) {
	t.Helper()
	w := actionWorkflow(t)
	_, secret, err := w.s.CreateAPIKey(t.Context(), "test client")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMCPHandler(w.s, w, nil))
	t.Cleanup(srv.Close)
	return w, srv.URL, secret
}
func mcpTestClient(t *testing.T, url, secret string) *client.Client {
	t.Helper()
	c, err := client.NewStreamableHttpClient(url+"/mcp", transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + secret}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err = c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1"}
	if _, err = c.Initialize(t.Context(), init); err != nil {
		t.Fatal(err)
	}
	return c
}
func mcpCall(t *testing.T, c *client.Client, name string, args map[string]any) json.RawMessage {
	t.Helper()
	var req mcp.CallToolRequest
	req.Params.Name, req.Params.Arguments = name, args
	result, err := c.CallTool(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("tool %s: %+v", name, result.Content)
	}
	b, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func mcpDecode[T any](t *testing.T, b []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestMCPRejectsMissingInvalidRevokedKeys(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	k, revoked, err := w.s.CreateAPIKey(t.Context(), "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.s.RevokeAPIKey(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key, path, origin string
		code              int
	}{
		{"", "/mcp", "", 401}, {"bad", "/mcp", "", 401}, {"jawa_" + strings.Repeat("0", 64), "/mcp", "", 401}, {revoked, "/mcp", "", 401},
		{secret, "/mcp", "https://evil.example", 403}, {secret, "/other", "", 404},
	} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", url+tc.path, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != tc.code {
			t.Fatalf("status %d want %d", res.StatusCode, tc.code)
		}
		if tc.code == 401 && res.Header.Get("WWW-Authenticate") != `Bearer realm="jawa"` {
			t.Fatal("missing challenge")
		}
	}
}
func TestMCPListsReadTools(t *testing.T) {
	_, url, secret := mcpTestServer(t)
	c := mcpTestClient(t, url, secret)
	tools, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_projects": true, "list_agents": true, "list_cards": true, "get_card": true, "wait_for_card": true}
	if len(tools.Tools) != len(want) {
		t.Fatal(tools.Tools)
	}
	for _, tool := range tools.Tools {
		if !want[tool.Name] || tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint {
			t.Fatalf("unexpected tool: %+v", tool)
		}
		delete(want, tool.Name)
	}
}
func TestAPIKeyStoreNeverPersistsSecret(t *testing.T) {
	w := actionWorkflow(t)
	k, secret, err := w.s.CreateAPIKey(t.Context(), " laptop ")
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 69 || k.Name != "laptop" || len(k.Prefix) != 13 {
		t.Fatal("invalid key format")
	}
	var id, name, hash, prefix string
	var created, used, revoked int64
	if err = w.s.db.QueryRow(`SELECT * FROM api_keys`).Scan(&id, &name, &hash, &prefix, &created, &used, &revoked); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{id, name, hash, prefix} {
		if strings.Contains(v, secret) {
			t.Fatal("persisted secret")
		}
	}
	if hash != tokenHash(secret) || used != 0 || revoked != 0 {
		t.Fatal("unexpected storage")
	}
	now := time.Now().Truncate(time.Second)
	for _, at := range []time.Time{now, now.Add(30 * time.Second)} {
		got, err := w.s.AuthenticateAPIKey(t.Context(), secret, at)
		if err != nil || got.ID != k.ID {
			t.Fatal(got, err)
		}
		if err = w.s.db.QueryRow(`SELECT last_used_at FROM api_keys WHERE id=?`, k.ID).Scan(&used); err != nil || used != now.Unix() {
			t.Fatal(used, err)
		}
	}
	if err = w.s.RevokeAPIKey(t.Context(), k.ID); err != nil {
		t.Fatal(err)
	}
	if w.s.RevokeAPIKey(t.Context(), k.ID) == nil {
		t.Fatal("second revoke succeeded")
	}
	if _, err = w.s.AuthenticateAPIKey(t.Context(), secret, now); err != errAPIKeyInvalid {
		t.Fatal(err)
	}
}
func TestAPIKeySettingsCreateShowsSecretOnceAndRevokes(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	form := url.Values{"_csrf": {csrf.Value}, "name": {"laptop"}, "view": {"settings"}}
	res := request(a, "POST", "/settings/apikeys", form, session, csrf)
	status(t, res, 201)
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("creation is cacheable")
	}
	secret := regexp.MustCompile(`jawa_[a-f0-9]{64}`).FindString(res.Body.String())
	if secret == "" {
		t.Fatal("missing secret")
	}
	res = request(a, "GET", "/settings", nil, session, csrf)
	status(t, res, 200)
	if strings.Contains(res.Body.String(), secret) || !strings.Contains(res.Body.String(), secret[:13]) || !strings.Contains(res.Body.String(), `id="mcp-access"`) {
		t.Fatal("settings key projection")
	}
	keys, err := a.s.APIKeys(t.Context())
	if err != nil || len(keys) != 1 {
		t.Fatal(keys, err)
	}
	status(t, request(a, "POST", "/settings/apikeys/revoke", url.Values{"_csrf": {csrf.Value}, "key_id": {keys[0].ID}}, session, csrf), 303)
	if _, err = a.s.AuthenticateAPIKey(t.Context(), secret, time.Now()); err != errAPIKeyInvalid {
		t.Fatal(err)
	}
	status(t, request(a, "POST", "/settings/apikeys", url.Values{"name": {"missing csrf"}}, session, csrf), 403)
	for _, name := range []string{"", strings.Repeat("x", 65)} {
		form.Set("name", name)
		if res = request(a, "POST", "/settings/apikeys", form, session, csrf); res.Code < 400 {
			t.Fatal("invalid name accepted")
		}
	}
}
func TestAPIKeyDatastarCreation(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	op := newOperationID()
	form := url.Values{"_csrf": {csrf.Value}, "name": {"desktop"}, "view": {"settings"}, "dialog_operation": {op}}
	res := sseRequest(a, "POST", "/settings/apikeys", form, session, csrf)
	body := assertSSE(t, res)
	secret := regexp.MustCompile(`jawa_[a-f0-9]{64}`).FindString(body)
	if secret == "" || !strings.Contains(body, "Copy your API key") || !strings.Contains(body, "selector #dialog-slot") || strings.Contains(body, "mode remove") || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing one-time dialog replacement")
	}
	next := sseRequest(a, "GET", "/snapshot?view=settings", nil, session, csrf)
	if strings.Contains(next.Body.String(), secret) {
		t.Fatal("secret in subsequent snapshot")
	}
}

func TestAPIKeyCreationSurvivesSnapshotFailure(t *testing.T) {
	w := actionWorkflow(t)
	// Break only the workspace read, leaving API-key persistence usable.
	if _, err := w.s.db.Exec(`DROP TABLE auto_resumes`); err != nil {
		t.Fatal(err)
	}
	a := &app{s: w.s}
	r := httptest.NewRequest("POST", "/settings/apikeys", strings.NewReader(url.Values{"name": {"desktop"}, "view": {"settings"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Datastar-Request", "true")
	res := httptest.NewRecorder()
	mutation := &mutationContext{}
	if err := a.mutateAPIKeyCreate(res, r, mutation); err != nil {
		t.Fatal(err)
	}
	body := assertSSE(t, res)
	secret := regexp.MustCompile(`jawa_[a-f0-9]{64}`).FindString(body)
	if secret == "" || !mutation.handled || !strings.Contains(body, "Copy your API key") || !strings.Contains(body, "selector #dialog-slot") || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("one-time secret lost on refresh failure")
	}
	if _, err := w.s.AuthenticateAPIKey(t.Context(), secret, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestMCPGetCardMatchesBoard(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	if err := planningDelivery(t, w, planningOutcome(t, w, c.ID, "Spec v1")); err != nil {
		t.Fatal(err)
	}
	cli := mcpTestClient(t, url, secret)
	raw := mcpCall(t, cli, "get_card", map[string]any{"card_id": c.ID})
	card := mcpDecode[mcpCard](t, raw)
	if card.Lane != "Planning" || card.Planning == nil || card.Planning.Revision != 1 || card.Planning.Spec != "Spec v1" || !card.Planning.CanApprove {
		t.Fatalf("%s", raw)
	}
	for _, forbidden := range []string{`"messages"`, `"discussion"`, `"turns"`, `"task_json"`, `"outcome"`, `"result"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("private field", forbidden)
		}
	}
	listed := mcpDecode[struct {
		Cards []mcpCardSummary `json:"cards"`
	}](t, mcpCall(t, cli, "list_cards", nil))
	if len(listed.Cards) != 1 || listed.Cards[0].Version != card.Version {
		t.Fatal("list/get versions differ")
	}
}
func TestMCPListCardsFilters(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	c := workflowCard(t, w.s)
	p, err := w.s.CreateProject("other")
	if err != nil {
		t.Fatal(err)
	}
	other, err := w.s.CreateCard(p.ID, "other", "description")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.s.SetCardArchived(other.ID, true); err != nil {
		t.Fatal(err)
	}
	cli := mcpTestClient(t, url, secret)
	projects := mcpDecode[struct {
		Projects []mcpProject `json:"projects"`
	}](t, mcpCall(t, cli, "list_projects", nil))
	if len(projects.Projects) != 2 {
		t.Fatal(projects)
	}
	for _, tc := range []struct {
		args  map[string]any
		count int
	}{
		{nil, 1}, {map[string]any{"include_archived": true}, 2}, {map[string]any{"project_id": c.ProjectID}, 1},
		{map[string]any{"project_id": p.ID}, 0}, {map[string]any{"lane": "Planning"}, 0}, {map[string]any{"lane": "Todo", "include_archived": true}, 2},
	} {
		got := mcpDecode[struct {
			Cards []mcpCardSummary `json:"cards"`
		}](t, mcpCall(t, cli, "list_cards", tc.args))
		if len(got.Cards) != tc.count {
			t.Fatal(tc.args, got)
		}
	}
}
func TestMCPWaitForCard(t *testing.T) {
	w, url, secret := mcpTestServer(t)
	c := workflowCard(t, w.s)
	cli := mcpTestClient(t, url, secret)
	card := mcpDecode[mcpCard](t, mcpCall(t, cli, "get_card", map[string]any{"card_id": c.ID}))
	args := map[string]any{"card_id": c.ID, "version": card.Version, "timeout_seconds": 1}
	type waited struct {
		Changed bool    `json:"changed"`
		Card    mcpCard `json:"card"`
	}
	got := mcpDecode[waited](t, mcpCall(t, cli, "wait_for_card", args))
	if got.Changed || got.Card.Version != card.Version {
		t.Fatal(got)
	}
	done := make(chan error, 1)
	go func() { time.Sleep(100 * time.Millisecond); done <- w.s.SetCardArchived(c.ID, true) }()
	start := time.Now()
	got = mcpDecode[waited](t, mcpCall(t, cli, "wait_for_card", args))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !got.Changed || !got.Card.Archived || time.Since(start) >= time.Second {
		t.Fatal(got)
	}
}
func TestMCPListAgentsWithoutPresence(t *testing.T) {
	_, url, secret := mcpTestServer(t)
	cli := mcpTestClient(t, url, secret)
	got := mcpDecode[struct {
		Available bool          `json:"available"`
		Agents    []AgentChoice `json:"agents"`
	}](t, mcpCall(t, cli, "list_agents", nil))
	if got.Available || got.Agents == nil || len(got.Agents) != 0 {
		t.Fatal(got)
	}
}
func TestWorkflowAgentsWithoutPresence(t *testing.T) {
	for _, w := range []*Workflow{nil, {}} {
		if _, err := w.Agents(context.Background()); err == nil {
			t.Fatal("missing discovery accepted")
		}
	}
}
func TestMCPKeyContext(t *testing.T) {
	k := APIKey{ID: "test-key"}
	ctx := context.WithValue(t.Context(), mcpKeyContext{}, k)
	if mcpKeyFromContext(ctx).ID != k.ID || mcpKeyFromContext(t.Context()).ID != "" {
		t.Fatal("key context identity")
	}
}

func TestMCPServerTimeoutPolicy(t *testing.T) {
	srv := newMCPHTTPServer(t.Context(), nil, nil, nil)
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 || srv.ReadHeaderTimeout != 5*time.Second || srv.IdleTimeout != 120*time.Second || srv.MaxHeaderBytes != 32<<10 {
		t.Fatal("incorrect MCP timeouts")
	}
}
func TestMCPBodyLimitAndEmptyOrigin(t *testing.T) {
	_, url, secret := mcpTestServer(t)
	for _, origin := range []bool{false, true} {
		req, err := http.NewRequestWithContext(t.Context(), "POST", url+"/mcp", strings.NewReader(strings.Repeat(" ", maxRequestBody+1)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if origin {
			req.Header.Set("Origin", "")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		if origin && res.StatusCode != 403 {
			t.Fatal(res.StatusCode)
		}
		if !origin && res.StatusCode < 400 {
			t.Fatal("oversize request accepted")
		}
	}
}
