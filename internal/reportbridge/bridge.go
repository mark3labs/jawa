package reportbridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	// Serialize intent admission so simultaneous terminal tools cannot race.
	submitMu sync.Mutex
	mu       sync.RWMutex
	runner   *runtime.Runner
	cfg      natschannel.Config
	conn     *nats.Conn
}

func (b *Bridge) Setup(ctx context.Context, k *kit.Kit, scope bonnie.RunScope) error {
	// An interrupted SDK step may lose its tool-call result. Replay our durable
	// intent BEFORE asking the model again, not by hoping it repeats the call.
	if err := b.recoverIntent(ctx, scope.RunID); err != nil {
		return err
	}
	k.AddTools(reporttools.Tools(reporttools.Binding{
		Identity: func(ctx context.Context) (reportprotocol.Identity, error) { return b.identity(ctx, scope.RunID) },
		Submit:   b.submit,
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
	var id reportprotocol.Identity
	for _, rec := range records {
		if rec.Kind != runtime.RecordExtensionData || rec.ExtType != "nats_task_admitted" {
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
	snapshot, err := r.Snapshot(ctx, runID)
	if err != nil {
		return id, err
	}
	if !r.IsActive(runID) || snapshot.State != runtime.RunRunning || snapshot.TurnID == "" {
		return id, errors.New("typed report requires the active runtime turn")
	}
	id.TurnID = snapshot.TurnID
	return id, nil
}

type reportChannel struct {
	bonnie.Channel
	bridge *Bridge
	cfg    natschannel.Config
	owned  *nats.Conn
}

func (c *reportChannel) Start(ctx context.Context) error {
	conn, owned, err := reportConnection(ctx, c.cfg)
	if err != nil {
		return err
	}
	c.owned = owned
	c.bridge.mu.Lock()
	c.bridge.conn = conn
	c.bridge.mu.Unlock()
	if err = c.Channel.(interface{ Start(context.Context) error }).Start(ctx); err != nil {
		if owned != nil {
			owned.Close()
		}
		return err
	}
	return nil
}
func (c *reportChannel) Shutdown(ctx context.Context) error {
	err := c.Channel.(interface{ Shutdown(context.Context) error }).Shutdown(ctx)
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
func (b *Bridge) Channel(cfg natschannel.Config) bonnie.Option {
	return bonnie.WithChannel(func(r *runtime.Runner) (bonnie.Channel, error) {
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
	})
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
	// A timeout is ambiguous, not permission to send a different terminal
	// report. Preserve the original request across retries/recovery turns.
	records, err := r.Journal().Replay(ctx, request.RunID)
	if err != nil {
		return reportprotocol.Ack{}, err
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
		if id == request.ReportID {
			if old.TaskID != request.TaskID || old.AgentID != request.AgentID || old.RunID != request.RunID || old.AttemptID != request.AttemptID {
				return reportprotocol.Ack{}, errors.New("report retry owner mismatch")
			}
			request = old
		} else if !resolved || ack.Accepted {
			return reportprotocol.Ack{}, errors.New("task already has a committed or unconfirmed terminal report; retry the identical payload")
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return reportprotocol.Ack{}, err
	}
	if _, err = r.Journal().Append(ctx, runtime.Record{RunID: request.RunID, Kind: runtime.RecordExtensionData, ExtType: intentType, Payload: data, Timestamp: time.Now().UTC()}); err != nil {
		return reportprotocol.Ack{}, err
	}
	ack, err := (reportprotocol.Client{Conn: conn, Root: root}).Submit(ctx, request)
	// Explicit rejections also resolve an intent; an unconfirmed transport does
	// not. A corrected report is allowed only after a definite rejection.
	if ack.ReportID == request.ReportID && ack.Version == reportprotocol.Version {
		receipt, _ := json.Marshal(ack)
		_, saveErr := r.Journal().Append(context.WithoutCancel(ctx), runtime.Record{RunID: request.RunID, Kind: runtime.RecordExtensionData, ExtType: receiptType, Payload: receipt, Timestamp: time.Now().UTC()})
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
			if _, saveErr := r.Journal().Append(context.WithoutCancel(ctx), runtime.Record{RunID: runID, Kind: runtime.RecordExtensionData, ExtType: receiptType, Payload: data, Timestamp: time.Now().UTC()}); saveErr != nil {
				return saveErr
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}
