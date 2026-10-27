package reportbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"jawa/internal/agentconcurrency"
	"jawa/internal/reportprotocol"
	"jawa/internal/reporttools"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	"github.com/mark3labs/bonnie/presence"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// Bridge is host configuration, never model input. Managed setup captures
// the runtime scope; admission correlation comes from BONNIE's durable journal.
type Bridge struct {
	gate               *agentconcurrency.Gate
	concurrencyDefault int
	// Serialize intent admission so simultaneous report tools cannot race.
	submitMu      sync.Mutex
	resumeWG      sync.WaitGroup
	resumeStopped bool
	// Protected by submitMu, from durable admission through receipt persistence.
	chatInFlight   map[string]bool
	resumeInFlight map[reportprotocol.FailedResume]bool
	mu             sync.RWMutex
	runner         *runtime.Runner
	cfg            natschannel.Config
	conn           *nats.Conn
}

func (b *Bridge) Setup(ctx context.Context, k *kit.Kit, scope bonnie.RunScope) (err error) {
	if b.gate != nil {
		if err = b.gate.Acquire(ctx); err != nil {
			return err
		}
		var once sync.Once
		release := func() { once.Do(b.gate.Release) }
		stop := context.AfterFunc(ctx, release)
		defer func() {
			if err != nil {
				stop()
				release()
			}
		}()
	}
	// A follow-up can resume an old suspension whose saved Context still
	// contains the original task. Overlay conversational scope on every model
	// request as well as enforcing report rejection independently below.
	b.mu.RLock()
	runner := b.runner
	b.mu.RUnlock()
	if runner != nil {
		records, err := runner.Journal().Replay(ctx, scope.RunID)
		if err != nil {
			return err
		}
		if chatFollowup(records) {
			k.OnContextPrepare(kit.HookPriorityNormal, func(h kit.ContextPrepareHook) *kit.ContextPrepareResult {
				msgs := append([]kit.LLMMessage(nil), h.Messages...)
				msgs = append(msgs, kit.LLMMessage{Role: "user", Content: []kit.LLMMessagePart{kit.LLMTextPart{Text: "Conversational follow-up only: previous task authority is revoked. Do not implement, approve, report workflow outcomes, or change external systems. Answer the latest operator message."}}})
				return &kit.ContextPrepareResult{Messages: msgs}
			})
		}
	}
	// An interrupted SDK step may lose its tool-call result. Replay our durable
	// intent BEFORE asking the model again, not by hoping it repeats the call.
	if err := b.recoverIntent(ctx, scope.RunID); err != nil {
		return err
	}
	// BONNIE's recovery hook stores only text, not typed FinalValue. Resolve
	// report calls from our durable receipt before it reconstructs tool pairs.
	if scope.Session != nil {
		if err := b.resolveReportTools(ctx, scope); err != nil {
			return err
		}
	}
	k.AddTools(reporttools.Tools(reporttools.Binding{
		Identity:   func(ctx context.Context) (reportprotocol.Identity, error) { return b.identity(ctx, scope.RunID) },
		Submit:     b.submit,
		FinalValue: suspensionValue,
		Prepare:    b.prepareRetry,
	})...)
	return nil
}

// BONNIE v0.22's status-index key is not exported. Keep this small adapter
// pinned to that version and test against an actual Channel admission. It
// duplicates cacheKey's documented encoding, including its long-key hash.
func admissionKey(c natschannel.Config) string {
	subject := c.Subject
	if subject == "" {
		subject = c.RootSubject + ".tasks"
	}
	stream := c.Stream
	if stream == "" {
		stream = natschannel.DefaultInputStreamName(subject)
	}
	consumer := c.Consumer
	if consumer == "" {
		consumer = natschannel.DefaultConsumerName(subject)
	}
	key := runtime.ReservedRunPrefix + "nats." + stream + "." + consumer + "." + c.AgentID + ".status." + hex.EncodeToString([]byte("index"))
	if len(key) > 200 {
		sum := sha256.Sum256([]byte(key))
		return runtime.ReservedRunPrefix + "nats.sha256." + hex.EncodeToString(sum[:])
	}
	return key
}
func (b *Bridge) identity(ctx context.Context, runID string) (reportprotocol.Identity, error) {
	id, err := b.admission(ctx, runID)
	if err != nil {
		return id, err
	}
	b.mu.RLock()
	r := b.runner
	b.mu.RUnlock()
	snapshot, err := r.Snapshot(ctx, runID)
	if err != nil {
		return id, err
	}
	if !r.IsActive(runID) || snapshot.State != runtime.RunRunning || snapshot.TurnID == "" {
		return id, errors.New("typed report requires the active runtime turn")
	}
	id.TurnID = snapshot.TurnID
	id.ToolCallID = kit.ToolCallIDFromContext(ctx)
	if id.ToolCallID == "" {
		return id, errors.New("typed report requires a trusted tool call ID")
	}
	return id, nil
}

// admission returns the run's trusted NATS task, without turn identity.
func (b *Bridge) admission(ctx context.Context, runID string) (reportprotocol.Identity, error) {
	b.mu.RLock()
	r, c := b.runner, b.cfg
	b.mu.RUnlock()
	if r == nil || c.AgentID == "" || c.RootSubject == "" {
		return reportprotocol.Identity{}, errors.New("typed reports require a managed NATS task")
	}
	records, err := r.Journal().Replay(ctx, admissionKey(c))
	if err != nil {
		return reportprotocol.Identity{}, errors.New("trusted NATS task admission unavailable")
	}
	chatRecords, chatErr := r.Journal().Replay(ctx, runID)
	if chatErr != nil || chatFollowup(chatRecords) {
		return reportprotocol.Identity{}, errors.New("conversational follow-up has no workflow authority")
	}
	var id reportprotocol.Identity
	for _, rec := range records {
		if rec.Kind != runtime.RecordExtensionData || rec.ExtType != admittedType {
			continue
		}
		var candidate reportprotocol.Identity
		if json.Unmarshal(rec.Payload, &candidate) != nil || candidate.RunID != runID || candidate.AgentID != c.AgentID {
			continue
		}
		if id.TaskID != "" && (id.TaskID != candidate.TaskID || id.AttemptID != candidate.AttemptID) {
			return id, errors.New("ambiguous trusted task admission")
		}
		id = candidate
	}
	if id.TaskID == "" || id.AttemptID == "" {
		return id, errors.New("run has no trusted NATS task admission")
	}
	return id, nil
}

type reportChannel struct {
	bonnie.Channel
	bridge        *Bridge
	cfg           natschannel.Config
	owned         *nats.Conn
	chatSub       *nats.Subscription
	resumeSub     *nats.Subscription
	resumeCancel  context.CancelFunc
	concurrencyWG sync.WaitGroup
	policyCancel  context.CancelFunc
}

func (c *reportChannel) Start(ctx context.Context) (err error) {
	conn, owned, err := reportConnection(ctx, c.cfg)
	if err != nil {
		return err
	}
	c.owned = owned
	policyCtx, policyCancel := context.WithCancel(ctx)
	c.policyCancel = policyCancel
	// A failed Start is not necessarily followed by Shutdown. Stop the policy
	// reader before closing its transport, including when a later setup fails.
	defer func() {
		if err != nil {
			policyCancel()
			c.concurrencyWG.Wait()
			c.bridge.mu.Lock()
			c.bridge.conn = nil
			c.bridge.mu.Unlock()
		}
	}()
	c.startConcurrency(policyCtx, conn)
	c.bridge.submitMu.Lock()
	c.bridge.resumeStopped = false
	c.bridge.submitMu.Unlock()
	c.bridge.mu.Lock()
	c.bridge.conn = conn
	c.bridge.mu.Unlock()
	// Repair the ACK-before-RecordSuspend crash window before BONNIE admits
	// recovery work. This never prompts the model or creates a new task/run.
	if err = c.bridge.restorePendingSuspensions(ctx); err != nil {
		if owned != nil {
			owned.Close()
		}
		return err
	}
	if err = c.Channel.(interface{ Start(context.Context) error }).Start(ctx); err != nil {
		if owned != nil {
			owned.Close()
		}
		return err
	}
	resumeCtx, cancel := context.WithCancel(ctx)
	c.resumeCancel = cancel
	if err = c.startResume(resumeCtx, conn); err != nil {
		cancel()
		_ = c.Channel.(interface{ Shutdown(context.Context) error }).Shutdown(ctx)
		if owned != nil {
			owned.Close()
		}
		return err
	}
	if err = c.startChat(resumeCtx, conn); err != nil {
		cancel()
		_ = c.resumeSub.Unsubscribe()
		_ = c.Channel.(interface{ Shutdown(context.Context) error }).Shutdown(ctx)
		if owned != nil {
			owned.Close()
		}
		return err
	}
	return nil
}
func (c *reportChannel) Shutdown(ctx context.Context) error {
	if c.policyCancel != nil {
		c.policyCancel()
	}
	c.concurrencyWG.Wait()
	if c.resumeCancel != nil {
		c.resumeCancel()
	}
	if c.chatSub != nil {
		_ = c.chatSub.Unsubscribe()
	}
	if c.resumeSub != nil {
		_ = c.resumeSub.Unsubscribe()
	}
	err := c.Channel.(interface{ Shutdown(context.Context) error }).Shutdown(ctx)
	c.bridge.submitMu.Lock()
	c.bridge.resumeStopped = true
	c.bridge.submitMu.Unlock()
	drained := make(chan struct{})
	go func() { c.bridge.resumeWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-ctx.Done():
		return ctx.Err()
	}
	if c.owned != nil {
		c.owned.Close()
	}
	c.bridge.mu.Lock()
	c.bridge.conn = nil
	c.bridge.mu.Unlock()
	return err
}

// Reuse authenticated caller-owned connections when present; otherwise mirror
// BONNIE's supported auth. Credentials never enter tools or model context.
func reportConnection(ctx context.Context, c natschannel.Config) (*nats.Conn, *nats.Conn, error) {
	if c.Conn != nil {
		return c.Conn, nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opts := []nats.Option{nats.Timeout(5 * time.Second)}
	if c.Token != "" {
		opts = append(opts, nats.Token(c.Token))
	}
	if c.Username != "" {
		opts = append(opts, nats.UserInfo(c.Username, c.Password))
	}
	if c.NKeySeed != "" {
		seed := c.NKeySeed
		key, err := nkeys.FromSeed([]byte(seed))
		if err != nil {
			return nil, nil, errors.New("invalid NATS report credentials")
		}
		pub, err := key.PublicKey()
		key.Wipe()
		if err != nil {
			return nil, nil, errors.New("invalid NATS report credentials")
		}
		opts = append(opts, nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
			k, err := nkeys.FromSeed([]byte(seed))
			if err != nil {
				return nil, err
			}
			defer k.Wipe()
			return k.Sign(nonce)
		}))
	}
	conn, err := nats.Connect(c.URL, opts...)
	if err != nil {
		return nil, nil, errors.New("NATS report connection failed; check transport and credentials")
	}
	return conn, conn, nil
}

// Compile-time reminder: report subjects must stay outside task routing.
func validReportRoot(root string) bool { return root != "" && !strings.ContainsAny(root, "*> \t\r\n") }

// Channel binds the runtime supplied by BONNIE's channel factory and mounts the
// trusted NATS adapter. Use the same Bridge's Setup via bonnie.WithKitSetup.
// New binds the trusted runtime and channel without starting a root BONNIE host.
// It is the same adapter used by Channel and permits embedded runtime tests.
func (b *Bridge) New(r *runtime.Runner, cfg natschannel.Config) (bonnie.Channel, error) {
	// BONNIE's worker pool is fixed at startup. The live gate in Setup bounds
	// actual model work across NATS, run chat, resumes and recovery.
	if b.gate != nil {
		cfg.Concurrency = agentconcurrency.Max
	}
	ch, err := natschannel.New(r, cfg)
	if err != nil {
		return nil, err
	}
	if !validReportRoot(cfg.RootSubject) {
		return nil, errors.New("jawa: typed reports require a literal NATS root subject")
	}
	b.mu.Lock()
	b.runner, b.cfg = r, cfg
	b.mu.Unlock()
	return &reportChannel{Channel: ch, bridge: b, cfg: cfg}, nil
}
func (b *Bridge) Channel(cfg natschannel.Config) bonnie.Option {
	return bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) { return b.New(r, cfg) })
}

func (c *reportChannel) AgentIdentity() string { return c.cfg.AgentID }
func (c *reportChannel) PresenceEndpoints() []presence.Endpoint {
	return c.Channel.(interface{ PresenceEndpoints() []presence.Endpoint }).PresenceEndpoints()
}

const intentType = "jawa_typed_report_intent_v1"
const receiptType = "jawa_typed_report_receipt_v1"

// Intent-before-send plus receipt-after-DB-ACK closes the crash window between
// external commit and KIT's atomic step write. These records are not prompt
// content and retain the original trusted turn for exact duplicate delivery.
func (b *Bridge) submit(ctx context.Context, request reportprotocol.Request) (reportprotocol.Ack, error) {
	b.submitMu.Lock()
	defer b.submitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return reportprotocol.Ack{}, err
	}
	b.mu.RLock()
	r, conn, root := b.runner, b.conn, b.cfg.RootSubject
	b.mu.RUnlock()
	if r == nil {
		return reportprotocol.Ack{}, errors.New("report runtime unavailable")
	}
	// A timeout is ambiguous, not permission to send a different
	// interaction. Preserve the original request across retries/recovery turns.
	records, err := r.Journal().Replay(ctx, request.RunID)
	if err != nil {
		return reportprotocol.Ack{}, err
	}
	if chatFollowup(records) {
		return reportprotocol.Ack{}, errors.New("conversational follow-up has no workflow authority")
	}
	prior := map[string]reportprotocol.Request{}
	receipts := map[string]reportprotocol.Ack{}
	for _, rec := range records {
		if rec.ExtType == intentType {
			var old reportprotocol.Request
			if json.Unmarshal(rec.Payload, &old) != nil || old.Validate() != nil {
				return reportprotocol.Ack{}, errors.New("invalid durable report intent")
			}
			prior[old.ReportID] = old
		}
		if rec.ExtType == receiptType {
			var ack reportprotocol.Ack
			if json.Unmarshal(rec.Payload, &ack) == nil {
				receipts[ack.ReportID] = ack
			}
		}
	}
	for id, old := range prior {
		ack, resolved := receipts[id]
		if old.ToolCallID == request.ToolCallID && id != request.ReportID {
			return reportprotocol.Ack{}, errors.New("tool interaction payload conflict")
		}
		if id == request.ReportID {
			if old.TaskID != request.TaskID || old.AgentID != request.AgentID || old.RunID != request.RunID || old.AttemptID != request.AttemptID || old.ToolCallID != request.ToolCallID {
				return reportprotocol.Ack{}, errors.New("report retry owner mismatch")
			}
			request = old
		} else if !resolved || ack.Terminal(old.Kind) {
			return reportprotocol.Ack{}, errors.New("task has a terminal report or unconfirmed interaction; retry the identical payload")
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return reportprotocol.Ack{}, err
	}
	if err = appendExt(ctx, r.Journal(), request.RunID, intentType, data); err != nil {
		return reportprotocol.Ack{}, err
	}
	ack, err := (reportprotocol.Client{Conn: conn, Root: root}).Submit(ctx, request)
	// Explicit rejections also resolve an intent; an unconfirmed transport does
	// not. A corrected report is allowed only after a definite rejection.
	if ack.ReportID == request.ReportID && ack.Version == reportprotocol.Version {
		receipt, _ := json.Marshal(ack)
		saveErr := appendExt(context.WithoutCancel(ctx), r.Journal(), request.RunID, receiptType, receipt)
		if saveErr != nil {
			return reportprotocol.Ack{}, saveErr
		}
	}
	return ack, err
}
func (b *Bridge) recoverIntent(ctx context.Context, runID string) error {
	b.mu.RLock()
	r, conn, root := b.runner, b.conn, b.cfg.RootSubject
	b.mu.RUnlock()
	if r == nil {
		return nil
	} // Non-NATS runs receive fail-closed tools.
	recs, err := r.Journal().Replay(ctx, runID)
	if err != nil {
		return err
	}
	if chatFollowup(recs) {
		return nil
	}
	intents := map[string]reportprotocol.Request{}
	resolved := map[string]bool{}
	for _, rec := range recs {
		if rec.ExtType == intentType {
			var request reportprotocol.Request
			if json.Unmarshal(rec.Payload, &request) != nil || request.Validate() != nil || request.RunID != runID {
				return errors.New("invalid durable typed report intent")
			}
			intents[request.ReportID] = request
		}
		if rec.ExtType == receiptType {
			var ack reportprotocol.Ack
			if json.Unmarshal(rec.Payload, &ack) != nil {
				return errors.New("invalid durable typed report receipt")
			}
			resolved[ack.ReportID] = true
		}
	}
	for id, request := range intents {
		if resolved[id] {
			continue
		}
		// Preserve the original turn identity and payload verbatim on recovery.
		ack, err := (reportprotocol.Client{Conn: conn, Root: root}).Submit(ctx, request)
		if ack.ReportID == request.ReportID && ack.Version == reportprotocol.Version {
			data, _ := json.Marshal(ack)
			if saveErr := appendExt(context.WithoutCancel(ctx), r.Journal(), runID, receiptType, data); saveErr != nil {
				return saveErr
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// KIT preserves dynamic FinalValue types; BONNIE accepts only its actual
// SuspendRequest value/pointer. Public after-tool hooks can only rewrite text.
// A blocker parks the run only when the orchestrator durably committed it as
// awaiting guidance; otherwise (older receivers) it remains terminal.
func suspensionValue(r reportprotocol.Request, ack reportprotocol.Ack) any {
	switch r.Kind {
	case reportprotocol.Clarify:
		return runtime.SuspendRequest{Kind: runtime.SuspendQuestion, Prompt: strings.Join(r.Payload.Questions, "\n"), TurnID: r.TurnID, ToolCallID: r.ToolCallID}
	case reportprotocol.ProposeSpec:
		return runtime.SuspendRequest{Kind: runtime.SuspendApproval, Prompt: r.Payload.Spec, Options: []string{"approve", "reject"}, TurnID: r.TurnID, ToolCallID: r.ToolCallID}
	case reportprotocol.ReportBlocker:
		if ack.AwaitGuidance {
			return runtime.SuspendRequest{Kind: runtime.SuspendQuestion, Prompt: r.Payload.Reason, TurnID: r.TurnID, ToolCallID: r.ToolCallID}
		}
		return r
	default:
		return r
	}
}

// resolveReportTools makes SDK transcript repair use the trusted durable ACK,
// rather than marking an already committed report's external effect unknown.
func (b *Bridge) resolveReportTools(ctx context.Context, scope bonnie.RunScope) error {
	b.mu.RLock()
	r := b.runner
	b.mu.RUnlock()
	if r == nil {
		return nil
	}
	recs, err := r.Journal().Replay(ctx, scope.RunID)
	if err != nil {
		return err
	}
	if chatFollowup(recs) {
		return nil
	}
	requests := map[string]reportprotocol.Request{}
	accepted := map[string]bool{}
	done := map[string]bool{}
	intents := map[string]bool{}
	for _, rec := range recs {
		switch rec.ExtType {
		case intentType:
			var req reportprotocol.Request
			if err := json.Unmarshal(rec.Payload, &req); err != nil {
				return err
			}
			requests[req.ReportID] = req
		case receiptType:
			var ack reportprotocol.Ack
			if err := json.Unmarshal(rec.Payload, &ack); err != nil {
				return err
			}
			accepted[ack.ReportID] = ack.Accepted
		}
		if rec.Kind == runtime.RecordToolIntent || rec.Kind == runtime.RecordToolOutcome {
			var call struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(rec.Payload, &call); err != nil {
				return err
			}
			if rec.Kind == runtime.RecordToolOutcome {
				done[call.ID] = true
			} else {
				intents[call.ID] = true
			}
		}
	}
	for id, req := range requests {
		if accepted[id] && intents[req.ToolCallID] && !done[req.ToolCallID] {
			if err := scope.Session.ResolveInterruptedTool(ctx, req.ToolCallID, "Typed report committed."); err != nil {
				return err
			}
		}
	}
	return nil
}

// restorePendingSuspensions runs while this host owns no active turns. Durable
// receipts identify what must wait; RecordResume identifies consumed requests.
func (b *Bridge) restorePendingSuspensions(ctx context.Context) error {
	b.mu.RLock()
	r := b.runner
	b.mu.RUnlock()
	if r == nil {
		return nil
	}
	runs, err := r.Journal().Runs(ctx, "")
	if err != nil {
		return err
	}
	for _, runID := range runs {
		if runtime.IsReservedRun(runID) {
			continue
		}
		state, err := r.Journal().State(ctx, runID)
		if err != nil {
			return err
		}
		if state == runtime.RunCompleted || state == runtime.RunFailed || state == runtime.RunCancelled || state == runtime.RunRetired {
			continue
		}
		if err := b.recoverIntent(ctx, runID); err != nil {
			return err
		}
		recs, err := r.Journal().Replay(ctx, runID)
		if err != nil {
			return err
		}
		if chatFollowup(recs) {
			continue
		}
		requests := map[string]reportprotocol.Request{}
		consumed := map[string]bool{}
		var pending *reportprotocol.Request
		var pendingAck reportprotocol.Ack
		var suspendedCall string
		for _, rec := range recs {
			switch rec.ExtType {
			case intentType:
				var req reportprotocol.Request
				if err := json.Unmarshal(rec.Payload, &req); err != nil {
					return err
				}
				requests[req.ReportID] = req
			case receiptType:
				var ack reportprotocol.Ack
				if err := json.Unmarshal(rec.Payload, &ack); err != nil {
					return err
				}
				req, exists := requests[ack.ReportID]
				if ack.Accepted && exists && !consumed[req.ToolCallID] {
					if ack.Suspends(req.Kind) {
						pending, pendingAck = &req, ack
					} else {
						pending = nil
					}
				}
			}
			if rec.Kind == runtime.RecordSuspend {
				var sus runtime.SuspendRequest
				if err := json.Unmarshal(rec.Payload, &sus); err != nil {
					return err
				}
				suspendedCall = sus.ToolCallID
			}
			if rec.Kind == runtime.RecordResume {
				consumed[suspendedCall] = true
				pending = nil
				suspendedCall = ""
			}
		}
		if pending == nil {
			continue
		}
		if suspendedCall != pending.ToolCallID {
			sus := suspensionValue(*pending, pendingAck).(runtime.SuspendRequest)
			data, _ := json.Marshal(sus)
			if _, err := r.Journal().Append(ctx, runtime.Record{RunID: runID, Kind: runtime.RecordSuspend, Timestamp: time.Now().UTC(), Text: sus.Prompt, Payload: data}); err != nil {
				return err
			}
		}
		if state != runtime.RunWaiting {
			// Recovery may have begun a fresh turn before the ACK was recovered.
			// Answers must target the original suspension's turn identity.
			var latestTurn string
			for _, rec := range recs {
				if rec.Kind == runtime.RecordTurn {
					latestTurn = rec.Text
				}
			}
			if latestTurn != pending.TurnID {
				if _, err := r.Journal().Append(ctx, runtime.Record{RunID: runID, Kind: runtime.RecordTurn, Text: pending.TurnID, Timestamp: time.Now().UTC()}); err != nil {
					return err
				}
			}
			if err := r.Journal().Checkpoint(ctx, runID, runtime.RunWaiting); err != nil {
				return err
			}
		}
	}
	return nil
}

// prepareRetry treats a model retry after transport ambiguity as delivery of
// the SAME interaction, not a new report. Resolved planning calls are never
// coalesced, so identical later questions/proposals still get distinct IDs.
func (b *Bridge) prepareRetry(ctx context.Context, request reportprotocol.Request) (reportprotocol.Request, error) {
	b.submitMu.Lock()
	defer b.submitMu.Unlock()
	b.mu.RLock()
	r := b.runner
	b.mu.RUnlock()
	if r == nil {
		return request, errors.New("report runtime unavailable")
	}
	recs, err := r.Journal().Replay(ctx, request.RunID)
	if err != nil {
		return request, err
	}
	if chatFollowup(recs) {
		return request, errors.New("follow-up has no workflow authority")
	}
	intents := map[string]reportprotocol.Request{}
	resolved := map[string]bool{}
	for _, rec := range recs {
		switch rec.ExtType {
		case intentType:
			var old reportprotocol.Request
			if json.Unmarshal(rec.Payload, &old) != nil || old.Validate() != nil {
				return request, errors.New("invalid durable report intent")
			}
			intents[old.ReportID] = old
		case receiptType:
			var ack reportprotocol.Ack
			if err := json.Unmarshal(rec.Payload, &ack); err != nil {
				return request, err
			}
			resolved[ack.ReportID] = true
		}
	}
	for id, old := range intents {
		if resolved[id] {
			continue
		}
		a, _ := json.Marshal(old.Payload)
		c, _ := json.Marshal(request.Payload)
		if old.Kind != request.Kind || string(a) != string(c) || old.TaskID != request.TaskID || old.AgentID != request.AgentID || old.RunID != request.RunID || old.AttemptID != request.AttemptID {
			return request, errors.New("unconfirmed interaction; retry the identical payload")
		}
		return old, nil
	}
	return request, nil
}
