package orchestrator

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type mcpStartupOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *mcpStartupOutput) Write(b []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(b)
}
func (o *mcpStartupOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.buf.String() }
func TestCommandMCPListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output := &mcpStartupOutput{}
	cmd := Command()
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs([]string{"--data-dir", t.TempDir(), "--listen", "127.0.0.1:0", "--nats-listen", "127.0.0.1:0", "--mcp-listen", addr})
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	ready := false
	client := &http.Client{Timeout: time.Second}
	for ctx.Err() == nil {
		req, err := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/mcp", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 401 {
				ready = true
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("early exit: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("shutdown timed out")
	}
	if !ready || !strings.Contains(output.String(), "MCP endpoint ready") {
		t.Fatal("MCP listener not ready")
	}
}
func TestSettingsMCPListenOverride(t *testing.T) {
	a := testApp(t)
	session, csrf := setupTest(t, a)
	form := url.Values{"_csrf": {csrf.Value}, "mcp-listen": {"127.0.0.1:8090"}}
	status(t, request(a, "POST", "/settings/orchestrator", form, session, csrf), 303)
	values, err := a.s.DashboardOverrides()
	if err != nil || values["mcp-listen"] != "127.0.0.1:8090" {
		t.Fatal(values, err)
	}
	form.Set("mcp-listen", "bad")
	if res := request(a, "POST", "/settings/orchestrator", form, session, csrf); res.Code < 400 {
		t.Fatal("invalid MCP address accepted")
	}
}
