package orchestrator

import (
	"context"
	"errors"
	"slices"

	protocol "github.com/mark3labs/bonnie/channel/nats"
	"github.com/nats-io/nats.go"
)

// Upgrade the embedded broker's v0.20 input stream in place. Retain the old
// subject so stored messages are not discarded; new tasks only use Agent routes.
func migrateAgentStream(ctx context.Context, conn *nats.Conn) error {
	js, err := conn.JetStream()
	if err != nil {
		return err
	}
	info, err := js.StreamInfo(protocol.DefaultInputStreamName("bonnie.tasks"), nats.Context(ctx))
	if errors.Is(err, nats.ErrStreamNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !slices.Contains(info.Config.Subjects, "bonnie.tasks.worker.*") || slices.Contains(info.Config.Subjects, "bonnie.tasks.agent.*") {
		return nil
	}
	cfg := info.Config
	cfg.Subjects = append(slices.Clone(cfg.Subjects), "bonnie.tasks.agent.*")
	_, err = js.UpdateStream(&cfg, nats.Context(ctx))
	return err
}
