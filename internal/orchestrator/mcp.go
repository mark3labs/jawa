package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const mcpEndpoint = "/mcp"

var mcpWaitPollInterval = time.Second

const mcpInstructions = `Jawa orchestrates coding agents on a kanban board.
Lanes: Todo -> Planning (optional) -> Building -> Done ("Ready for review": a verified PR); Fix repairs an existing PR.
Agents run asynchronously. Write tools only queue or record work and return at once.
To follow progress, call get_card, or wait_for_card with the card's last "version", until lane/status changes.
Only the information shown on cards and the card/plan page is available; run transcripts are not.`

type mcpService struct {
	s           *Store
	w           *Workflow
	logger      *log.Logger
	issueClient *http.Client //nolint:unused // Reserved provider stub injection point for issue #106.
}

type mcpKeyContext struct{}

func mcpKeyFromContext(ctx context.Context) APIKey {
	k, _ := ctx.Value(mcpKeyContext{}).(APIKey)
	return k
}
func (m *mcpService) log() *log.Logger {
	if m.logger != nil {
		return m.logger
	}
	return log.Default()
}

func newMCPHTTPServer(ctx context.Context, s *Store, w *Workflow, logger *log.Logger) *http.Server {
	return &http.Server{
		BaseContext:       func(net.Listener) context.Context { return ctx },
		Handler:           newMCPHandler(s, w, logger),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		// No ReadTimeout/WriteTimeout: each tool bounds its own work.
	}
}
func newMCPHandler(s *Store, w *Workflow, logger *log.Logger) http.Handler {
	return (&mcpService{s: s, w: w, logger: logger}).handler()
}
func (m *mcpService) handler() http.Handler {
	srv := server.NewMCPServer("jawa", "1", server.WithToolCapabilities(false), server.WithRecovery(), server.WithInstructions(mcpInstructions))
	m.registerReadTools(srv)
	transport := server.NewStreamableHTTPServer(srv, server.WithEndpointPath(mcpEndpoint), server.WithStateLess(true), server.WithDisableLocalhostProtection(true))
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-store")
		rw.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != mcpEndpoint {
			http.NotFound(rw, r)
			return
		}
		// Reject even an explicitly empty Origin header: browser clients are unsupported.
		if _, present := r.Header["Origin"]; present {
			http.Error(rw, "browser origins are not allowed", http.StatusForbidden)
			return
		}
		secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		key, err := APIKey{}, errAPIKeyInvalid
		if ok {
			authCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			key, err = m.s.AuthenticateAPIKey(authCtx, strings.TrimSpace(secret), time.Now())
			cancel()
		}
		if err != nil {
			if !errors.Is(err, errAPIKeyInvalid) {
				m.log().Error("MCP authentication unavailable")
				http.Error(rw, "authentication unavailable", http.StatusServiceUnavailable)
				return
			}
			m.log().Warn("MCP authentication rejected", "remote", r.RemoteAddr)
			rw.Header().Set("WWW-Authenticate", `Bearer realm="jawa"`)
			http.Error(rw, "invalid or missing API key", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(rw, r.Body, maxRequestBody)
		transport.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), mcpKeyContext{}, key)))
	})
}

// mcpJSON includes structured content and a JSON text block for older clients.
func mcpJSON(v any) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultStructured(v, string(b)), nil
}
