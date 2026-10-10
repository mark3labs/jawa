// factory-smoke runs an isolated, offline browser E2E against the production
// orchestrator Command and BONNIE host. Only the language model is scripted.
// Run from the repository root: go run ./cmd/factory-smoke
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"jawa/internal/orchestrator"
)

const workerID = "factory-smoke-worker"
const fixturePR = `{"pr_number":1}`

// This is a real Kit provider, not a replacement runtime.Agent or a protocol
// consumer. Hold the turn open long enough for the browser to observe running.
type model struct{}

func (model) answer(ctx context.Context) error {
	log.Print("BONNIE Kit model pickup (offline scripted final; no tools)")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return nil
	}
}
func (m model) Generate(ctx context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	if err := m.answer(ctx); err != nil {
		return nil, err
	}
	return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: fixturePR}}, FinishReason: fantasy.FinishReasonStop}, nil
}
func (m model) Stream(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
	if err := m.answer(ctx); err != nil {
		return nil, err
	}
	return iter.Seq[fantasy.StreamPart](func(yield func(fantasy.StreamPart) bool) {
		for _, part := range []fantasy.StreamPart{
			{Type: fantasy.StreamPartTypeTextStart, ID: "final"},
			{Type: fantasy.StreamPartTypeTextDelta, ID: "final", Delta: fixturePR},
			{Type: fantasy.StreamPartTypeTextEnd, ID: "final"},
			{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop},
		} {
			if !yield(part) {
				return
			}
		}
	}), nil
}
func (model) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("smoke: unexpected structured model call")
}
func (model) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("smoke: unexpected structured model call")
}
func (model) Provider() string { return "smoke" }
func (model) Model() string    { return "final" }

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func address() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		return "", err
	}
	return addr, nil
}
func run() (runErr error) {
	// Never inherit provider credentials; the production verifier must fail closed
	// before opening any HTTP connection. Kit discovery is disabled below too.
	for _, key := range []string{"JAWA_GITHUB_TOKEN", "GITHUB_TOKEN", "JAWA_FORGEJO_TOKEN"} {
		if err := os.Unsetenv(key); err != nil {
			return err
		}
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "jawa-factory-smoke-")
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, os.RemoveAll(dir)) }()
	// BONNIE loads .env relative to cwd before Kit exists. An empty private
	// cwd prevents any repository/user dotenv or tree discovery from being read.
	if err := os.Chdir(dir); err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, os.Chdir(root)) }()
	httpAddr, err := address()
	if err != nil {
		return err
	}
	natsAddr, err := address()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := orchestrator.Command()
	cmd.SetArgs([]string{"--data-dir", filepath.Join(dir, "server"), "--listen", httpAddr, "--nats-listen", natsAddr})
	serverDone := make(chan error, 1)
	go func() { serverDone <- cmd.ExecuteContext(ctx) }()
	defer func() {
		cancel()
		if err := <-serverDone; err != nil {
			log.Printf("orchestrator shutdown: %v", err)
		}
	}()
	client := &http.Client{Timeout: time.Second}
	base := "http://" + httpAddr
	ready := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		resp, e := client.Get(base + "/setup")
		if e == nil {
			_ = resp.Body.Close() // Best-effort cleanup of the readiness probe.
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		return errors.New("orchestrator HTTP not ready")
	}

	// Loopback-only authenticated control bridge lets the browser rotate worker
	// credentials FIRST, then start/stop the real worker without backend hooks.
	user, pass, controlToken := "smoke-worker", secret(), secret()
	var mu sync.Mutex
	var stopWorker context.CancelFunc
	var workerDone chan error
	stop := func() error {
		if stopWorker == nil {
			return nil
		}
		stopWorker()
		e := <-workerDone
		stopWorker = nil
		return e
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if err := stop(); err != nil {
			log.Printf("worker shutdown: %v", err)
		}
	}()
	control, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	bridge := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer "+controlToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		// Seed only the command-owned temporary database, after browser setup and
		// project creation. Direct SQL avoids rerunning workflow migrations and
		// represents an old Building card with no attempt, not a fake outcome.
		if r.URL.Path == "/seed-legacy" {
			db, e := sql.Open("sqlite", filepath.Join(dir, "server", "orchestrator.db"))
			if e == nil {
				defer func() {
					if err := db.Close(); err != nil {
						log.Printf("close smoke fixture database: %v", err)
					}
				}()
				var result sql.Result
				result, e = db.Exec(`INSERT INTO cards(id,project_id,title,description,status,position,created_at,issue_url)
				SELECT 'smoke-legacy',id,'Legacy Building card','Pre-attempt legacy fixture','Building',0,?,'' FROM projects WHERE name='Smoke project'`, time.Now().Unix())
				if e == nil {
					var n int64
					n, e = result.RowsAffected()
					if n != 1 {
						e = errors.New("expected one fixture project")
					}
				}
			}
			if e != nil {
				http.Error(w, "legacy fixture failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/stop" {
			if e := stop(); e != nil {
				http.Error(w, "worker shutdown failed", http.StatusInternalServerError)
				return
			}
			log.Print("BONNIE worker stopped; presence unregistered")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path != "/start" || stopWorker != nil {
			http.Error(w, "invalid control", http.StatusBadRequest)
			return
		}
		nc, e := nats.Connect("nats://"+natsAddr, nats.UserInfo(user, pass), nats.Timeout(time.Second))
		if e != nil {
			http.Error(w, "worker authentication failed", http.StatusInternalServerError)
			return
		}
		wc, wcancel := context.WithCancel(ctx)
		registry, e := presencenats.New(wc, nc, presencenats.Config{Bucket: "jawa_workers", TTL: 30 * time.Second, Create: true})
		if e != nil {
			wcancel()
			nc.Close()
			http.Error(w, "presence failed", http.StatusInternalServerError)
			return
		}
		// Observe publishes without acknowledging or manufacturing protocol messages.
		_, e = nc.Subscribe("bonnie.>", func(msg *nats.Msg) {
			if msg.Subject == "bonnie.tasks" || msg.Subject == "bonnie.events" || msg.Subject == "bonnie.results" {
				log.Printf("BONNIE publish subject=%s", msg.Subject)
			}
		})
		if e != nil {
			wcancel()
			nc.Close()
			http.Error(w, "observer failed", http.StatusInternalServerError)
			return
		}
		agent := bonnie.New(
			bonnie.WithName(workerID), bonnie.WithAddr("127.0.0.1:0"), bonnie.WithWebUI(false),
			bonnie.WithJournal(filepath.Join(dir, "worker")), bonnie.WithInstructions(""), bonnie.WithSkills(""), bonnie.WithContextFiles(""),
			bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(filepath.Join(dir, "workspaces")))), bonnie.WithoutHumanInput(),
			bonnie.WithKit(func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
				kit.WithProvider("smoke", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
					return &kit.ProviderResult{Model: model{}}, nil
				})(o)
				kit.WithModel("smoke/final")(o)
			}),
			bonnie.WithChannel(func(runner *runtime.Runner) (bonnie.Channel, error) {
				return natschannel.New(runner, natschannel.Config{Conn: nc, RootSubject: "bonnie", WorkerID: workerID, TargetedTasks: true, CreateStream: true})
			}),
			bonnie.WithPresence(bonnie.PresenceConfig{Registry: registry, WorkerID: workerID}),
		)
		stopWorker = wcancel
		workerDone = make(chan error, 1)
		go func() { e := agent.Run(wc); nc.Close(); workerDone <- e }()
		log.Print("BONNIE host starting with production NATS channel and presence")
		w.WriteHeader(http.StatusNoContent)
	})}
	bridgeDone := make(chan error, 1)
	go func() { bridgeDone <- bridge.Serve(control) }()
	defer func() {
		runErr = errors.Join(runErr, bridge.Close())
		if err := <-bridgeDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = errors.Join(runErr, fmt.Errorf("control bridge: %w", err))
		}
	}()

	// Playwright owns assertions. Propagate its exit code, and always tear down
	// the worker before Command closes its workflow, broker and private store.
	node := exec.CommandContext(ctx, "node", "smoke.mjs")
	node.Dir = filepath.Join(root, "internal", "orchestrator", "frontend")
	node.Env = append(os.Environ(), "JAWA_SMOKE_URL="+base, "JAWA_SMOKE_CONTROL=http://"+control.Addr().String(), "JAWA_SMOKE_CONTROL_TOKEN="+controlToken, "JAWA_SMOKE_WORKER_USER="+user, "JAWA_SMOKE_WORKER_PASSWORD="+pass, "JAWA_SMOKE_SCREENSHOT="+filepath.Join(root, "node_modules", ".factory-smoke", "desktop.png"))
	node.Stdout, node.Stderr = os.Stdout, os.Stderr
	if err := node.Run(); err != nil {
		return fmt.Errorf("browser smoke: %w", err)
	}
	return nil
}
func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
