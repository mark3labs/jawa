package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mark3labs/bonnie/presence"
	"github.com/nats-io/nats.go"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
)

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func tokenHash(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func CSRFToken(r *http.Request) string {
	c, e := r.Cookie("csrf")
	if e != nil {
		return ""
	}
	return c.Value
}

type loginAttempts struct {
	count int
	until time.Time
}
type app struct {
	s               *Store
	issueClient     *http.Client
	logger          *log.Logger
	debugDefault    bool
	runtimeDefaults map[string]string
	workflow        *Workflow
	ns              *server.Server
	options         *server.Options // Latest successfully applied options; guarded by mu.
	mu              sync.Mutex
	attempts        map[string]loginAttempts
}

const maxRequestBody = 1 << 20

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// A root-scoped worker must be public, including before login. Never cache
	// this script in HTTP caches: installations should discover new versions.
	if r.URL.Path == "/sw.js" {
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/assets/sw.js"
		r2.URL = &u
		http.FileServer(http.FS(uiAssets)).ServeHTTP(w, r2)
		return
	}
	if r.URL.Path == "/assets/manifest.webmanifest" {
		w.Header().Set("Content-Type", "application/manifest+json")
	}
	// Embedded FS contains the assets directory, so preserve that prefix.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		// Fixed URLs remain public (including worker precaches), but must not
		// reuse stale HTTP cache entries across embedded-asset deployments.
		w.Header().Set("Cache-Control", "no-cache")
		http.FileServer(http.FS(uiAssets)).ServeHTTP(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				a.responseError(w, r, "Request too large. Please shorten the form and try again.", http.StatusRequestEntityTooLarge)
			} else {
				a.responseError(w, r, "Invalid form. Please refresh and try again.", http.StatusBadRequest)
			}
			return
		}
	}
	switch r.URL.Path {
	case "/setup":
		a.setup(w, r)
		return
	case "/login":
		a.login(w, r)
		return
	}
	if !a.authed(r) {
		if requiresAuthenticatedResponse(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ready, err := a.s.NATSConfigured()
	if err != nil {
		a.responseError(w, r, "Unable to check NATS setup. Please try again.", http.StatusServiceUnavailable)
		return
	}
	if !ready && r.Method == http.MethodGet && r.URL.Path != "/settings" && r.URL.Path != "/events" && r.URL.Path != "/snapshot" {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		http.Redirect(w, r, "/board", http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/runs/") {
		a.runDetailRoute(w, r)
		return
	}
	if kind, ok := viewFromPath(r.URL.Path); ok && r.Method == http.MethodGet {
		a.ensureCSRF(w, r)
		renderWorkflowPage(w, r, a.s, a.workflow, parseViewParams(r, kind), a.runtimeDefaults)
		return
	}
	if r.URL.Path == "/activity" && r.Method == http.MethodGet {
		a.activity(w, r)
		return
	}
	if r.URL.Path == "/events" || r.URL.Path == "/snapshot" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			a.responseError(w, r, "Method not allowed. Use GET to refresh the workspace.", http.StatusMethodNotAllowed)
			return
		}
		a.ensureCSRF(w, r)
		if r.URL.Path == "/events" {
			a.events(w, r)
		} else {
			a.snapshot(w, r, false, mutationEffects{})
		}
		return
	}
	if strings.HasPrefix(r.URL.Path, "/dialogs/") {
		a.dialogBody(w, r)
		return
	}
	route, ok := mutations[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.serveMutation(w, r, route, ready)
}

// Streaming and enhanced clients need an error rather than an HTML login redirect.
func requiresAuthenticatedResponse(r *http.Request) bool {
	return r.URL.Path == "/events" || r.URL.Path == "/snapshot" || (strings.HasPrefix(r.URL.Path, "/runs/") && strings.HasSuffix(r.URL.Path, "/events")) || datastarRequest(r)
}

const sessionLifetime = 30 * 24 * time.Hour

func (a *app) ensureCSRF(w http.ResponseWriter, r *http.Request) {
	if len(CSRFToken(r)) == 64 {
		return
	}
	c := &http.Cookie{Name: "csrf", Value: newID(), Path: "/", Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: time.Now().Add(sessionLifetime), MaxAge: int(sessionLifetime / time.Second)}
	http.SetCookie(w, c)
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, old := range cookies {
		if old.Name != "csrf" {
			r.AddCookie(old)
		}
	}
	r.AddCookie(c)
}
func (a *app) csrf(w http.ResponseWriter, r *http.Request) bool {
	c, e := r.Cookie("csrf")
	v := r.PostForm.Get("_csrf")
	if v == "" {
		v = r.Header.Get("X-CSRF-Token")
	}
	if e != nil || len(c.Value) != 64 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(v)) != 1 {
		a.responseError(w, r, "Invalid CSRF token. Refresh the page and try again.", http.StatusForbidden)
		return false
	}
	return true
}
func (a *app) authed(r *http.Request) bool {
	c, e := r.Cookie("session")
	if e != nil {
		return false
	}
	exp, e := a.s.SessionExpiry(tokenHash(c.Value))
	return e == nil && exp > time.Now().Unix()
}
func (a *app) clear(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("session"); e == nil {
		_ = a.s.DeleteSession(tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
}
func (a *app) issue(w http.ResponseWriter, r *http.Request) error {
	t := newID()
	exp := time.Now().Add(sessionLifetime)
	if err := a.s.CreateSession(tokenHash(t), exp.Unix()); err != nil {
		return err
	}
	if c, e := r.Cookie("session"); e == nil {
		_ = a.s.DeleteSession(tokenHash(c.Value))
	}
	_ = a.s.PruneSessions(time.Now().Unix())
	http.SetCookie(w, &http.Cookie{Name: "session", Value: t, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: exp, MaxAge: int(sessionLifetime / time.Second)})
	http.SetCookie(w, &http.Cookie{Name: "csrf", Value: newID(), Path: "/", Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: exp, MaxAge: int(sessionLifetime / time.Second)})
	return nil
}
func (a *app) setup(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "POST" {
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	ok, err := a.s.HasAdmin()
	if err != nil {
		http.Error(w, "database unavailable", http.StatusInternalServerError)
		return
	}
	if ok {
		if r.Method == "GET" {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		} else {
			http.Error(w, "setup already complete", http.StatusConflict)
		}
		return
	}
	if r.Method == "GET" {
		a.ensureCSRF(w, r)
		authPageWithRequest(w, r, "setup", "")
		return
	}
	if !a.csrf(w, r) {
		return
	}
	p := r.FormValue("password")
	user := strings.TrimSpace(r.FormValue("username"))
	if user == "" {
		user = "admin"
	}
	if len(p) < 12 || len(p) > 72 || len(user) > 128 {
		http.Error(w, "invalid username or password (password must be 12–72 bytes)", http.StatusBadRequest)
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), passwordHashCost)
	if err != nil {
		http.Error(w, "unable to create account", http.StatusInternalServerError)
		return
	}
	err = a.s.CreateAdmin(user, string(h))
	if err != nil {
		http.Error(w, "setup could not be completed", http.StatusConflict)
		return
	}
	if err = a.issue(w, r); err != nil {
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

var dummyLoginHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("not-a-real-account-password"), passwordHashCost)
	return h
}()

func (a *app) allowLogin(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if a.attempts == nil {
		a.attempts = make(map[string]loginAttempts)
	}
	for k, v := range a.attempts {
		if !now.Before(v.until) {
			delete(a.attempts, k)
		}
	}
	v := a.attempts[host]
	if v.count >= 5 {
		return false
	}
	if v.count == 0 {
		if len(a.attempts) >= 4096 {
			return false
		}
		v.until = now.Add(time.Minute)
	}
	v.count++
	a.attempts[host] = v
	return true
}
func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		ok, err := a.s.HasAdmin()
		if err != nil {
			http.Error(w, "database unavailable", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		a.ensureCSRF(w, r)
		authPageWithRequest(w, r, "login", "")
		return
	}
	if r.Method != "POST" {
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if !a.csrf(w, r) {
		return
	}
	if !a.allowLogin(r) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many login attempts", http.StatusTooManyRequests)
		return
	}
	user, h, err := a.s.AdminCredentials()
	hash := []byte(h)
	if err != nil {
		hash = dummyLoginHash
	}
	passwordOK := bcrypt.CompareHashAndPassword(hash, []byte(r.FormValue("password"))) == nil
	input := strings.TrimSpace(r.FormValue("username"))
	if input == "" {
		input = "admin"
	}
	if err != nil || !passwordOK || subtle.ConstantTimeCompare([]byte(user), []byte(input)) != 1 {
		w.WriteHeader(401)
		authPageWithRequest(w, r, "login", "Invalid username or password")
		return
	}
	if err = a.issue(w, r); err != nil {
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Never start the listener until persisted bcrypt credentials are available.
func natsOptions(s *Store, listen, dir string) (*server.Options, error) {
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("invalid NATS port")
	}
	if port == 0 {
		port = -1
	}
	var hash string
	err = s.db.QueryRow("SELECT value FROM settings WHERE key='nats_password'").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		if err = s.saveNATS("nats://"+listen, "worker", newID()); err != nil {
			return nil, err
		}
		err = s.db.QueryRow("SELECT value FROM settings WHERE key='nats_password'").Scan(&hash)
	}
	if err != nil {
		return nil, err
	}
	if _, err = bcrypt.Cost([]byte(hash)); err != nil {
		return nil, fmt.Errorf("invalid stored NATS password hash")
	}
	cfg := s.NATSConfig()
	if strings.TrimSpace(cfg.Username) == "" || cfg.Username == "jawa-internal" {
		return nil, fmt.Errorf("NATS username required")
	}
	return &server.Options{Host: host, Port: port, Username: cfg.Username, Password: hash, JetStream: true, StoreDir: filepath.Join(dir, "nats"), NoSigs: true}, nil
}
func (a *app) rotateNATS(url, user, pass string) error {
	user = strings.TrimSpace(user)
	if user == "jawa-internal" || user == "" || len(user) > 128 || len(pass) < 12 || len(pass) > 72 {
		return fmt.Errorf("username and 12–72 byte password required")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ns != nil && a.options == nil {
		return fmt.Errorf("NATS options unavailable")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), passwordHashCost)
	if err != nil {
		return err
	}
	var next *server.Options
	err = a.s.rotateNATSSettings(url, user, string(hash), func() error {
		if a.ns != nil {
			next = a.options.Clone()
			if len(next.Users) > 0 {
				for i, u := range next.Users {
					if u.Username != "jawa-internal" {
						next.Users[i] = &server.User{Username: user, Password: string(hash), Permissions: workerPermissions()}
					}
				}
				next.Username, next.Password = "", ""
			} else {
				next.Username, next.Password = user, string(hash)
			}
			if err = a.ns.ReloadOptions(next); err != nil {
				return fmt.Errorf("NATS reload failed")
			}
		}
		return nil
	}, func() {
		if a.ns != nil {
			if rollbackErr := a.ns.ReloadOptions(a.options.Clone()); rollbackErr != nil {
				a.ns.Shutdown()
			}
		}
	})
	if err != nil {
		return err
	}
	if next != nil {
		a.options = next
	}
	return nil
}
func Command() *cobra.Command {
	var dir, listen, natsListen, mcpListen, decisionModel string
	var debug bool
	providerDefaults := map[string]string{}
	cmd := &cobra.Command{Use: "orchestrator", Short: "Run the local orchestrator", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		logger := log.NewWithOptions(cmd.ErrOrStderr(), log.Options{ReportTimestamp: true, Prefix: "orchestrator"})
		if debug {
			logger.SetLevel(log.DebugLevel)
		}
		logger.Info("Starting orchestrator")
		runtimeDefaults := map[string]string{"listen": listen, "nats-listen": natsListen, "mcp-listen": mcpListen, "debug": strconv.FormatBool(debug), "allow-no-ci": cmd.Flags().Lookup("allow-no-ci").Value.String()}
		for _, key := range []string{"debug", "listen", "nats-listen", "mcp-listen"} {
			if cmd.Flags().Changed(key) {
				runtimeDefaults[key] = cmd.Flags().Lookup(key).Value.String()
			}
		}
		debugDefault := debug
		if dir == "" {
			base, err := os.UserConfigDir()
			if err != nil {
				return err
			}
			dir = filepath.Join(base, "jawa")
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		logger.Info("Opening store", "data_dir", dir)
		s, err := OpenStore(filepath.Join(dir, "orchestrator.db"))
		if err != nil {
			return err
		}
		readProviderFlags(cmd, providerDefaults)
		s.runtimeDefaults = providerDefaults
		maps.Copy(runtimeDefaults, providerDefaults)
		defer func() {
			if closeErr := s.Close(); closeErr != nil {
				logger.Error("Failed to close store", "err", closeErr)
			}
		}()
		overrides, err := s.DashboardOverrides()
		if err != nil {
			return err
		}
		if v := overrides["listen"]; v != "" {
			listen = v
		}
		if v := overrides["nats-listen"]; v != "" {
			natsListen = v
		}
		if v := overrides["mcp-listen"]; v != "" {
			mcpListen = v
		}
		if mcpListen != "" {
			if err := validatedListenAddress(mcpListen); err != nil {
				return err
			}
		}
		if v := overrides["debug"]; v == "true" {
			logger.SetLevel(log.DebugLevel)
		} else if v == "false" {
			logger.SetLevel(log.InfoLevel)
		}
		if err := validatedListenAddress(listen); err != nil {
			return err
		}
		if err := validatedListenAddress(natsListen); err != nil {
			return err
		}
		if decisionModel != "" {
			if err := CheckDecisionModel(decisionModel); err != nil {
				return err
			}
		}
		if value, ok := overrides["debug"]; ok {
			debug = value == "true"
		}
		if debug {
			logger.SetLevel(log.DebugLevel)
		} else {
			logger.SetLevel(log.InfoLevel)
		}
		opts, err := natsOptions(s, natsListen, dir)
		if err != nil {
			return err
		}
		// Internal credentials are process-local and independent of agent rotation.
		systemPassword := newID()
		systemHash, err := bcrypt.GenerateFromPassword([]byte(systemPassword), passwordHashCost)
		if err != nil {
			return err
		}
		opts.Users = []*server.User{{Username: opts.Username, Password: opts.Password, Permissions: workerPermissions()}, {Username: "jawa-internal", Password: string(systemHash)}}
		opts.Username, opts.Password = "", ""
		ns, err := server.NewServer(opts.Clone())
		if err != nil {
			return err
		}
		go ns.Start()
		defer func() { ns.Shutdown(); ns.WaitForShutdown() }()
		if !ns.ReadyForConnections(10 * time.Second) {
			return fmt.Errorf("NATS server failed to start")
		}
		logger.Info("NATS server ready", "address", ns.ClientURL())
		opts = opts.Clone()
		opts.Port = ns.Addr().(*net.TCPAddr).Port
		conn, err := nats.Connect(ns.ClientURL(), nats.InProcessServer(ns), nats.UserInfo("jawa-internal", systemPassword),
			nats.DisconnectErrHandler(func(_ *nats.Conn, _ error) { logger.Warn("Workflow NATS connection disconnected") }),
			nats.ReconnectHandler(func(_ *nats.Conn) { logger.Info("Workflow NATS connection reconnected") }),
			nats.ClosedHandler(func(_ *nats.Conn) { logger.Info("Workflow NATS connection closed") }),
			nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, _ error) { logger.Error("Workflow NATS asynchronous error") }))
		if err != nil {
			return err
		}
		defer conn.Close()
		logger.Info("Starting workflow")
		workflow, err := newWorkflow(cmd.Context(), s, conn, logger)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := workflow.Close(); closeErr != nil {
				logger.Error("Failed to close workflow", "err", closeErr)
			}
		}()
		workflow.SetProviderChecks(ProviderVerifier(s, nil), ProviderConflictChecker(s, nil))
		workflow.SetAutoMerger(s)
		workflow.SetProviderMerger(ProviderPRMerger(s, nil))
		workflow.SetProviderIdentity(ProviderIdentity(s, nil))
		if decisionModel != "" {
			workflow.SetFailureClassifier(DecisionFailureClassifier(decisionModel, nil))
			logger.Info("Failure triage enabled", "decision_model", decisionModel)
		}
		logger.Info("Workflow ready", "provider_verification", true)
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		srv := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Addr: listen, Handler: &app{s: s, ns: ns, options: opts, workflow: workflow, logger: logger, debugDefault: debugDefault, runtimeDefaults: runtimeDefaults}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			logger.Error("Failed to listen for HTTP", "address", listen, "err", err)
			return err
		}
		defer func() {
			if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				logger.Error("Failed to close HTTP listener", "err", closeErr)
			}
		}()
		done := make(chan error, 1)
		go func() { done <- srv.Serve(listener) }()
		logger.Info("Orchestrator ready", "http_address", listener.Addr().String())
		var mcpSrv *http.Server
		var mcpDone chan error
		if mcpListen != "" {
			mcpListener, listenErr := net.Listen("tcp", mcpListen)
			if listenErr != nil {
				logger.Error("Failed to listen for MCP", "address", mcpListen, "err", listenErr)
				_ = srv.Close()
				if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
					logger.Error("HTTP server stopped unexpectedly", "err", serveErr)
				}
				return listenErr
			}
			mcpSrv = newMCPHTTPServer(ctx, s, workflow, logger)
			mcpDone = make(chan error, 1)
			go func() { mcpDone <- mcpSrv.Serve(mcpListener) }()
			logger.Info("MCP endpoint ready", "address", "http://"+mcpListener.Addr().String()+mcpEndpoint)
		}
		shutdown := func(server *http.Server, finished <-chan error) error {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			shutdownErr := server.Shutdown(shutdownCtx)
			if shutdownErr != nil {
				_ = server.Close()
			}
			if serveErr := <-finished; serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				return errors.Join(shutdownErr, serveErr)
			}
			return shutdownErr
		}
		select {
		case err = <-mcpDone:
			_ = shutdown(srv, done)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case err = <-done:
			if mcpSrv != nil {
				_ = shutdown(mcpSrv, mcpDone)
			}
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			logger.Info("Shutting down orchestrator")
			err = shutdown(srv, done)
			if mcpSrv != nil {
				err = errors.Join(err, shutdown(mcpSrv, mcpDone))
			}
			if err != nil {
				logger.Error("HTTP shutdown failed", "err", err)
			} else {
				logger.Info("HTTP server stopped")
			}
			return err
		}
	}}
	cmd.Flags().BoolVar(&debug, "debug", orchestratorEnv("JAWA_ORCHESTRATOR_DEBUG", "false") == "true", "Include debug logs for polling and ignored deliveries")
	cmd.Flags().StringVar(&dir, "data-dir", orchestratorEnv("JAWA_ORCHESTRATOR_DATA_DIR", ""), "Persistent data directory")
	cmd.Flags().StringVar(&listen, "listen", orchestratorEnv("JAWA_ORCHESTRATOR_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	cmd.Flags().StringVar(&natsListen, "nats-listen", orchestratorEnv("JAWA_ORCHESTRATOR_NATS_LISTEN", "127.0.0.1:4222"), "Authenticated embedded NATS listen address")
	cmd.Flags().StringVar(&mcpListen, "mcp-listen", orchestratorEnv("JAWA_ORCHESTRATOR_MCP_LISTEN", ""), "MCP (Streamable HTTP) listen address; empty disables it. Requires an API key; put TLS in front")
	cmd.Flags().StringVar(&decisionModel, "decision-model", orchestratorEnv("JAWA_ORCHESTRATOR_DECISION_MODEL", ""), "Decision model (provider/model) that triages failures before automatic resume; empty disables triage")
	providerFlags(cmd, providerDefaults)
	cmd.AddCommand(ReconcileCommand(), ForceStopCommand())
	return cmd
}

func orchestratorEnv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func validatedListenAddress(address string) error {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return errors.New("port must be between 0 and 65535")
	}
	return nil
}

func (a *app) activity(w http.ResponseWriter, r *http.Request) {
	agents := []presence.Record{}
	attempts := []Attempt{}
	var err error
	if a.workflow != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		agents, err = a.workflow.Agents(ctx)
		cancel()
		if err != nil {
			http.Error(w, "presence unavailable", http.StatusServiceUnavailable)
			return
		}
		if agents == nil {
			agents = []presence.Record{}
		}
		attempts, err = a.workflow.Attempts()
		if err != nil {
			http.Error(w, "attempts unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	cards, err := a.s.Cards("")
	if err != nil {
		http.Error(w, "cards unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Agents   []presence.Record `json:"agents"`
		Attempts []Attempt         `json:"attempts"`
		Cards    []Card            `json:"cards"`
	}{agents, attempts, cards})
}

// refreshPRChecks gives the bounded provider batch time to finish before writing
// the normal finite SSE response or native redirect. No global timeout changes.
func (a *app) refreshPRChecks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(35 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		a.responseError(w, r, "Unable to refresh PR status. Try again.", http.StatusServiceUnavailable)
		return
	}
	var result PRRefreshResult
	var err error
	if a.workflow == nil {
		err = ErrPRChecksUnavailable
	} else {
		result, err = a.workflow.RefreshPRChecks(ctx, r.FormValue("project_id"), r.FormValue("card_id"))
	}
	if err != nil {
		status, message := http.StatusServiceUnavailable, "Unable to refresh PR status. Unfinished checks remain queued; no clear status is assumed. Try again."
		switch {
		case errors.Is(err, ErrPRRefreshBusy):
			status, message = http.StatusConflict, "PR checks are already running. Wait for the current check, then try again."
		case errors.Is(err, ErrPRChecksUnavailable):
			message = "PR checks are unavailable. Try again when the provider checker is available."
		case errors.Is(err, sql.ErrNoRows):
			status, message = http.StatusNotFound, "Project or verified Done pull request not found. Refresh the board and try again."
		}
		// A failed batch may have invalidated old evidence. Patch the current board
		// before the error notice rather than leaving an old clear status onscreen.
		if datastarRequest(r) && ctx.Err() == nil {
			a.snapshot(w, r, false, mutationEffects{})
		}
		a.responseError(w, r, message, status)
		return
	}
	if datastarRequest(r) {
		a.snapshot(w, r, true, mutationEffects{flash: fmt.Sprintf("PR status refresh: %d targeted, %d checked, %d queued for background checking.", result.Targeted, result.Checked, result.Queued)})
		return
	}
	http.Redirect(w, r, parseViewParams(r, "").Path(), http.StatusSeeOther)
}

// Attribution is derived from a currently valid server session, never the form.
func (a *app) requestOperator(r *http.Request) (string, error) {
	c, err := r.Cookie("session")
	if err != nil {
		return "", errors.New("authenticated operator required")
	}
	user, err := a.s.SessionOperator(r.Context(), tokenHash(c.Value), time.Now().Unix())
	if err != nil || strings.TrimSpace(user) == "" {
		return "", errors.New("authenticated operator required")
	}
	return user, nil
}
