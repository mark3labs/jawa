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
	"log"
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
	s        *Store
	workflow *Workflow
	ns       *server.Server
	options  *server.Options // Latest successfully applied options; guarded by mu.
	mu       sync.Mutex
	attempts map[string]loginAttempts
}

const maxRequestBody = 1 << 20

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	// Embedded FS contains the assets directory, so preserve that prefix.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
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
		if r.URL.Path == "/events" || r.URL.Path == "/snapshot" || datastarRequest(r) {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		http.Redirect(w, r, "/board", http.StatusSeeOther)
		return
	}
	if kind, ok := viewFromPath(r.URL.Path); ok && r.Method == http.MethodGet {
		a.ensureCSRF(w, r)
		renderWorkflowPage(w, r, a.s, a.workflow, parseViewParams(r, kind))
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
	switch r.URL.Path {
	case "/logout", "/projects", "/cards", "/cards/move", "/cards/retry", "/cards/reset", "/cards/delete", "/cards/cancel", "/settings/nats":
	default:
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		a.responseError(w, r, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	if !a.csrf(w, r) {
		return
	}
	var err error
	var fx mutationEffects
	switch r.URL.Path {
	case "/logout":
		a.clear(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	case "/projects":
		var project Project
		project, err = a.s.CreateProjectDetails(r.FormValue("name"), strings.ToLower(r.FormValue("provider")), r.FormValue("repo"), r.FormValue("base_branch"))
		if err == nil {
			fx.goTo = boardPath(project.ID)
		}
	case "/cards":
		_, err = a.s.CreateCardDetails(r.FormValue("project_id"), r.FormValue("title"), r.FormValue("description"), r.FormValue("issue_url"))
	case "/cards/move":
		raw := r.FormValue("position")
		pos := 0
		if raw != "" {
			for _, c := range raw {
				if c < '0' || c > '9' {
					a.responseError(w, r, "Invalid card position. Refresh the board and try again.", http.StatusBadRequest)
					return
				}
			}
			pos, err = strconv.Atoi(raw)
			if err != nil {
				a.responseError(w, r, "Invalid card position. Refresh the board and try again.", http.StatusBadRequest)
				return
			}
		}
		if a.workflow == nil {
			err = errors.New("workflow is unavailable")
		} else {
			err = a.workflow.MoveCard(r.FormValue("card_id"), r.FormValue("status"), pos)
		}
	case "/cards/retry":
		if a.workflow == nil {
			err = errors.New("workflow unavailable")
		} else {
			err = a.workflow.Retry(r.FormValue("card_id"))
		}
	case "/cards/reset", "/cards/delete":
		if a.workflow == nil {
			err = errors.New("workflow unavailable")
		} else if r.URL.Path == "/cards/reset" {
			err = a.workflow.ResetCard(r.FormValue("card_id"))
		} else if r.FormValue("acknowledge_orphan") == "yes" {
			err = a.workflow.DeleteUnreconciledCard(r.FormValue("card_id"))
		} else {
			err = a.workflow.DeleteCard(r.FormValue("card_id"))
		}
	case "/cards/cancel":
		if a.workflow == nil {
			err = errors.New("workflow unavailable")
		} else {
			err = a.workflow.CancelCard(r.Context(), r.FormValue("card_id"))
			if err == nil {
				fx.flash = "Cancellation was requested; it is not confirmed until the agent reports cancellation. External actions already taken cannot be undone."
			}
		}
	case "/settings/nats":
		err = a.rotateNATS(r.FormValue("url"), r.FormValue("username"), r.FormValue("password"))
		if err == nil {
			fx.flash = "Agent credentials rotated. Agents using the old password were disconnected."
		}
	}
	if err != nil {
		a.responseError(w, r, "Unable to apply request: "+strings.TrimRight(err.Error(), ". \t\n")+". Refresh the board and try again.", http.StatusBadRequest)
		return
	}
	if datastarRequest(r) {
		a.snapshot(w, r, true, fx)
		return
	}
	// Native form fallback: return to the screen the form came from.
	back := parseViewParams(r, "")
	if fx.goTo != "" {
		http.Redirect(w, r, fx.goTo, http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back.Path(), http.StatusSeeOther)
}
func (a *app) ensureCSRF(w http.ResponseWriter, r *http.Request) {
	if len(CSRFToken(r)) == 64 {
		return
	}
	c := &http.Cookie{Name: "csrf", Value: newID(), Path: "/", Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 86400}
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
	var exp int64
	e = a.s.db.QueryRow("SELECT expires_at FROM sessions WHERE token_hash=?", tokenHash(c.Value)).Scan(&exp)
	return e == nil && exp > time.Now().Unix()
}
func (a *app) clear(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("session"); e == nil {
		_, _ = a.s.db.Exec("DELETE FROM sessions WHERE token_hash=?", tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
}
func (a *app) issue(w http.ResponseWriter, r *http.Request) error {
	t := newID()
	exp := time.Now().Add(24 * time.Hour)
	if _, err := a.s.db.Exec("INSERT INTO sessions(token_hash,expires_at) VALUES(?,?)", tokenHash(t), exp.Unix()); err != nil {
		return err
	}
	if c, e := r.Cookie("session"); e == nil {
		_, _ = a.s.db.Exec("DELETE FROM sessions WHERE token_hash=?", tokenHash(c.Value))
	}
	_, _ = a.s.db.Exec("DELETE FROM sessions WHERE expires_at<=?", time.Now().Unix())
	http.SetCookie(w, &http.Cookie{Name: "session", Value: t, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: exp, MaxAge: 86400})
	http.SetCookie(w, &http.Cookie{Name: "csrf", Value: newID(), Path: "/", Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, Expires: exp, MaxAge: 86400})
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
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "unable to create account", http.StatusInternalServerError)
		return
	}
	_, err = a.s.db.Exec("INSERT INTO admin(id,username,password_hash) VALUES(1,?,?)", user, string(h))
	if err != nil {
		http.Error(w, "setup could not be completed", http.StatusConflict)
		return
	}
	if err = a.issue(w, r); err != nil {
		http.Error(w, "unable to create session", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var dummyLoginHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("not-a-real-account-password"), bcrypt.DefaultCost)
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
	var user, h string
	err := a.s.db.QueryRow("SELECT username,password_hash FROM admin WHERE id=1").Scan(&user, &h)
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
		if err = s.saveNATS("nats://"+listen, "admin", newID()); err != nil {
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
	if strings.TrimSpace(cfg.Username) == "" {
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
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := a.s.db.Begin()
	if err != nil {
		return err
	}
	// Rollback is best-effort; after Commit it returns sql.ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	for _, kv := range [][2]string{{"nats_url", url}, {"nats_username", user}, {"nats_password", string(hash)}} {
		if _, err = tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", kv[0], kv[1]); err != nil {
			return err
		}
	}
	var next *server.Options
	if a.ns != nil {
		next = a.options.Clone()
		if len(next.Users) > 0 {
			for i, u := range next.Users {
				if u.Username != "jawa-internal" {
					next.Users[i] = &server.User{Username: user, Password: string(hash)}
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
	if err = tx.Commit(); err != nil {
		if a.ns != nil {
			if rollbackErr := a.ns.ReloadOptions(a.options.Clone()); rollbackErr != nil {
				a.ns.Shutdown()
			}
		}
		return err
	}
	if next != nil {
		a.options = next
	}
	return nil
}
func Command() *cobra.Command {
	var dir, listen, natsListen string
	cmd := &cobra.Command{Use: "orchestrator", Short: "Run the local orchestrator", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if _, _, err := net.SplitHostPort(listen); err != nil {
			return err
		}
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
		s, err := OpenStore(filepath.Join(dir, "orchestrator.db"))
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := s.Close(); closeErr != nil {
				log.Printf("close orchestrator store: %v", closeErr)
			}
		}()
		opts, err := natsOptions(s, natsListen, dir)
		if err != nil {
			return err
		}
		// Internal credentials are process-local and independent of agent rotation.
		systemPassword := newID()
		systemHash, err := bcrypt.GenerateFromPassword([]byte(systemPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		opts.Users = []*server.User{{Username: opts.Username, Password: opts.Password}, {Username: "jawa-internal", Password: string(systemHash)}}
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
		opts = opts.Clone()
		opts.Port = ns.Addr().(*net.TCPAddr).Port
		conn, err := nats.Connect(ns.ClientURL(), nats.InProcessServer(ns), nats.UserInfo("jawa-internal", systemPassword))
		if err != nil {
			return err
		}
		defer conn.Close()
		workflow, err := NewWorkflow(cmd.Context(), s, conn)
		if err != nil {
			return err
		}
		defer func() { _ = workflow.Close() }()
		workflow.SetVerifier(ProviderVerifier(s, nil))
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		srv := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Addr: listen, Handler: &app{s: s, ns: ns, options: opts, workflow: workflow}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
		done := make(chan error, 1)
		go func() { done <- srv.ListenAndServe() }()
		select {
		case err = <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = srv.Shutdown(shutdownCtx)
			if err != nil {
				_ = srv.Close()
			}
			<-done
			return err
		}
	}}
	cmd.Flags().StringVar(&dir, "data-dir", "", "Persistent data directory")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	cmd.Flags().StringVar(&natsListen, "nats-listen", "127.0.0.1:4222", "Authenticated embedded NATS listen address")
	cmd.AddCommand(ReconcileCommand())
	return cmd
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
