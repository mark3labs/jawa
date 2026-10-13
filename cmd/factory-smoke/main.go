// factory-smoke runs an isolated, offline browser E2E against the production
// orchestrator Command and BONNIE host. Only the language model is scripted.
// Run from the repository root: go run ./cmd/factory-smoke
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	natsapi "github.com/mark3labs/bonnie/channel/natsapi"
	"iter"
	"jawa/internal/runapitoken"
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

	"github.com/mark3labs/bonnie/sandbox"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"jawa/internal/orchestrator"
	"jawa/internal/reportbridge"
	"jawa/internal/testmodel"

	"strings"
)

const agentID = "factory-smoke-agent"
const fixturePR = `{"pr_number":1}`

// This is a real Kit provider, not a replacement runtime.Agent or a protocol
// consumer. Hold the turn open long enough for the browser to observe running.
type model struct{ planning *planningScript }
type planningScript struct {
	mu   sync.Mutex
	step int
}

func (model) answer(ctx context.Context) error {
	log.Print("BONNIE Kit model pickup (offline scripted completeTask tool)")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return nil
	}
}
func (m model) reply(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	if strings.Contains(fmt.Sprint(call.Prompt), "RUN_DETAIL_FOLLOWUP_FIXTURE") {
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Run detail follow-up response."}}, FinishReason: fantasy.FinishReasonStop}, nil
	}
	if m.planning == nil {
		if err := m.answer(ctx); err != nil {
			return nil, err
		}
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "smoke-complete", ToolName: "completeTask", Input: fixturePR}}, FinishReason: fantasy.FinishReasonToolCalls}, nil
	}
	// Old cancelled queued tasks may still be admitted. They must not consume the
	// current task's fixture script; natural text cannot revive their DB authority.
	if !strings.Contains(fmt.Sprint(call.Prompt), "Resumable planning smoke") {
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Cancelled fixture task finished."}}, FinishReason: fantasy.FinishReasonStop}, nil
	}
	if strings.Contains(fmt.Sprint(call.Prompt), "Implement the approved specification") {
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: "planning-build-complete", ToolName: "completeTask", Input: fixturePR}}, FinishReason: fantasy.FinishReasonToolCalls}, nil
	}
	m.planning.mu.Lock()
	defer m.planning.mu.Unlock()
	m.planning.step++
	name, input := "", ""
	switch m.planning.step {
	case 1:
		name, input = "clarify", `{"questions":["Which API should the plan use?"]}`
	case 2:
		name, input = "proposeSpec", `{"spec":"## Draft plan\nUse API v2","repo_commit":"`+strings.Repeat("a", 40)+`"}`
	case 3:
		name, input = "proposeSpec", `{"spec":"## Reviewed plan\nUse API v3","repo_commit":"`+strings.Repeat("a", 40)+`"}`
	case 4:
		return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: "Planning approved and finished."}}, FinishReason: fantasy.FinishReasonStop}, nil
	case 5:
		name, input = "completeTask", fixturePR
	default:
		return nil, errors.New("smoke: planning script exhausted")
	}
	return &fantasy.Response{Content: fantasy.ResponseContent{fantasy.ToolCallContent{ToolCallID: fmt.Sprintf("planning-%d", m.planning.step), ToolName: name, Input: input}}, FinishReason: fantasy.FinishReasonToolCalls}, nil
}
func (m model) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	return m.reply(ctx, call)
}
func (m model) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	response, err := m.reply(ctx, call)
	if err != nil {
		return nil, err
	}
	return iter.Seq[fantasy.StreamPart](func(yield func(fantasy.StreamPart) bool) {
		for _, content := range response.Content {
			switch c := content.(type) {
			case fantasy.ToolCallContent:
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: c.ToolCallID, ToolCallName: c.ToolName, ToolCallInput: c.Input}) {
					return
				}
			case fantasy.TextContent:
				for _, part := range []fantasy.StreamPart{{Type: fantasy.StreamPartTypeTextStart, ID: "text"}, {Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: c.Text}, {Type: fantasy.StreamPartTypeTextEnd, ID: "text"}} {
					if !yield(part) {
						return
					}
				}
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: response.FinishReason})
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

	// Loopback-only authenticated control bridge lets the browser rotate agent
	// credentials FIRST, then start/stop the real agent without backend hooks.
	user, pass, controlToken := "smoke-agent", secret(), secret()
	var mu sync.Mutex
	var stopAgent context.CancelFunc
	var agentDone chan error
	var resumeModel *testmodel.Model
	stop := func() error {
		if stopAgent == nil {
			return nil
		}
		stopAgent()
		e := <-agentDone
		stopAgent = nil
		return e
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		if err := stop(); err != nil {
			log.Printf("agent shutdown: %v", err)
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
		if r.URL.Path == "/resume-snapshot" {
			writeResumeSnapshot(w, r, dir, resumeModel)
			return
		}
		if r.URL.Path == "/seed-resume-context" {
			if stopAgent != nil {
				http.Error(w, "host must be stopped", 400)
				return
			}
			if e := seedResumeContext(r.Context(), dir, r.URL.Query().Get("card")); e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
				http.Error(w, "agent shutdown failed", http.StatusInternalServerError)
				return
			}
			log.Print("BONNIE agent stopped; presence unregistered")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if (r.URL.Path != "/start" && r.URL.Path != "/start-planning" && r.URL.Path != "/start-failing" && r.URL.Path != "/start-resume-build" && r.URL.Path != "/start-resume-planning") || stopAgent != nil {
			http.Error(w, "invalid control", http.StatusBadRequest)
			return
		}
		nc, e := nats.Connect("nats://"+natsAddr, nats.UserInfo(user, pass), nats.Timeout(time.Second))
		if e != nil {
			http.Error(w, "agent authentication failed", http.StatusInternalServerError)
			return
		}
		wc, wcancel := context.WithCancel(ctx)
		registry, e := presencenats.New(wc, nc, presencenats.Config{Bucket: "jawa_agents", TTL: 30 * time.Second, Create: true})
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
		fixtureModel := model{}
		if r.URL.Path == "/start-planning" {
			fixtureModel.planning = &planningScript{}
		}
		resumeModel = nil
		switch r.URL.Path {
		case "/start-failing":
			resumeModel = testmodel.New(testmodel.Call("shell", `{"command":"printf resume-workspace-marker > resume-marker.txt"}`))
		case "/start-resume-build":
			resumeModel = testmodel.New(testmodel.Call("shell", `{"command":"test \"$(cat resume-marker.txt)\" = resume-workspace-marker && printf workspace-preserved"}`), testmodel.Call("completeTask", fixturePR))
		case "/start-resume-planning":
			resumeModel = testmodel.New(testmodel.Call("shell", `{"command":"test \"$(cat resume-marker.txt)\" = resume-workspace-marker && printf workspace-preserved"}`), testmodel.Call("clarify", `{"questions":["Resume browser API?"]}`), testmodel.Call("proposeSpec", `{"spec":"## Resumed browser plan","repo_commit":"`+strings.Repeat("a", 40)+`"}`))
		}
		selectedResumeModel := resumeModel
		js, err := nc.JetStream()
		if err != nil {
			wcancel()
			nc.Close()
			http.Error(w, "API fixture setup failed", 500)
			return
		}
		kv, err := js.KeyValue(runapitoken.Bucket)
		if err != nil {
			wcancel()
			nc.Close()
			http.Error(w, "API fixture setup failed", 500)
			return
		}
		entry, err := kv.Get(runapitoken.PublicKey)
		if err != nil {
			wcancel()
			nc.Close()
			http.Error(w, "API fixture setup failed", 500)
			return
		}
		pub := ed25519.PublicKey(entry.Value())
		apiSubject, _ := runapitoken.Subject("bonnie", agentID)
		var reportRuntime reportbridge.Bridge
		agent := bonnie.New(
			bonnie.WithName(agentID), bonnie.WithAddr("127.0.0.1:0"), bonnie.WithWebUI(false),
			bonnie.WithJournal(filepath.Join(dir, "agent")), bonnie.WithInstructions(""), bonnie.WithSkills(""), bonnie.WithContextFiles(""),
			bonnie.WithSandbox(sandbox.Local(sandbox.WithLocalRoot(filepath.Join(dir, "workspaces")))), bonnie.WithoutHumanInput(),
			bonnie.WithKit(func(o *kit.Options) {
				o.SkipConfig, o.NoContextFiles, o.NoSkills, o.NoExtensions, o.NoAgents, o.Quiet = true, true, true, true, true, true
				kit.WithProvider("smoke", func(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
					return &kit.ProviderResult{Model: fixtureModel}, nil
				})(o)
				kit.WithModel("smoke/final")(o)
				if selectedResumeModel != nil {
					selectedResumeModel.Option()(o)
				}
			}),
			bonnie.WithKitSetup(reportRuntime.Setup),
			reportRuntime.Channel(natschannel.Config{Conn: nc, RootSubject: "bonnie", AgentID: agentID, TargetedTasks: true, CreateStream: true}),
			bonnie.WithPresence(bonnie.PresenceConfig{Registry: registry, AgentID: agentID, Labels: map[string]string{"jawa.api_subject": apiSubject}}),
			bonnie.WithNATSAPI(natsapi.Config{Conn: nc, Subject: apiSubject, Authenticator: func(r *http.Request) (*channel.Principal, error) {
				token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
				if !ok || runapitoken.Verify(pub, token, agentID, time.Now()) != nil {
					return nil, bonniehttp.ErrUnauthenticated
				}
				return &channel.Principal{ID: "orchestrator", Kind: "user", Authenticator: "jawa-orchestrator"}, nil
			}}),
		)
		stopAgent = wcancel
		agentDone = make(chan error, 1)
		go func() { e := agent.Run(wc); nc.Close(); agentDone <- e }()
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
	// the agent before Command closes its workflow, broker and private store.
	node := exec.CommandContext(ctx, "node", "smoke.mjs")
	node.Dir = filepath.Join(root, "internal", "orchestrator", "frontend")
	node.Env = append(os.Environ(), "JAWA_SMOKE_URL="+base, "JAWA_SMOKE_CONTROL=http://"+control.Addr().String(), "JAWA_SMOKE_CONTROL_TOKEN="+controlToken, "JAWA_SMOKE_AGENT_USER="+user, "JAWA_SMOKE_AGENT_PASSWORD="+pass, "JAWA_SMOKE_SCREENSHOT="+filepath.Join(root, "node_modules", ".factory-smoke", "desktop.png"))
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
