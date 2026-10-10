package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"github.com/mark3labs/bonnie/runtime"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/spf13/cobra"
)

// JAWA_NATS_PRESENCE=true opts into JetStream KV agent discovery. All agents
// in a discovery scope must use the same JAWA_NATS_PRESENCE_BUCKET (default
// jawa_agents). The bucket has a 30s TTL, refreshed by BONNIE every 10s.
// JAWA_NATS_CREATE_STREAM also permits bucket creation; otherwise provision it
// ahead of time with a matching TTL. Registration/conflict/refresh errors stop
// serving rather than silently running without presence.
//
// New only validates the NATS channel; its private connection is opened in
// Start. Open a shared caller-owned connection in PreRunE instead. RunE's defer
// closes it AFTER Agent.Run has drained channels and unregistered presence,
// including on startup failure. Do not rely on PostRunE (skipped on errors) or
// a defer around Serve (which may os.Exit). Help and other commands never dial.
func configureServing(cmd *cobra.Command, agent *bonnie.Agent) {
	cleanup := func() {}
	servingFlagsE(cmd, func(name string, cfg natschannel.Config) error {
		if os.Getenv("JAWA_NATS_PRESENCE") != "true" {
			agent.Configure(bonnie.WithName(name), withNATS(cfg))
			return nil
		}
		shared, pc, closeConn, err := openNATSPresence(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		cleanup = closeConn
		agent.Configure(bonnie.WithName(name), withNATS(shared), bonnie.WithPresence(pc))
		return nil
	})
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		defer func() { cleanup() }()
		return run(cmd, args)
	}
}

func natsAuthConfig(c natschannel.Config) natschannel.Config {
	if c.Conn == nil {
		for field, key := range map[*string]string{
			&c.URL: "NATS_URL", &c.NKeySeed: "NATS_NKEY_SEED",
			&c.Token: "NATS_TOKEN", &c.Username: "NATS_USERNAME", &c.Password: "NATS_PASSWORD",
		} {
			if *field == "" {
				*field = os.Getenv(key)
			}
		}
	}
	return c
}

func openNATSPresence(ctx context.Context, cfg natschannel.Config) (natschannel.Config, bonnie.PresenceConfig, func(), error) {
	c := natsAuthConfig(cfg)
	// Reuse BONNIE's validation before dialing (including conflicting auth,
	// URL credentials, agent ID and subject validation). New does not connect.
	if _, err := natschannel.New(&runtime.Runner{}, c); err != nil {
		return cfg, bonnie.PresenceConfig{}, nil, err
	}
	if err := ctx.Err(); err != nil {
		return cfg, bonnie.PresenceConfig{}, nil, err
	}
	closeConn := func() {}
	if c.Conn == nil {
		opts := []nats.Option{nats.Timeout(5 * time.Second)}
		if c.NKeySeed != "" {
			// Recreate and wipe the decoded key on every reconnect signature.
			seed := c.NKeySeed
			key, err := nkeys.FromSeed([]byte(seed))
			if err != nil {
				return cfg, bonnie.PresenceConfig{}, nil, errors.New("jawa: invalid NATS NKey seed")
			}
			pub, err := key.PublicKey()
			key.Wipe()
			if err != nil {
				return cfg, bonnie.PresenceConfig{}, nil, errors.New("jawa: invalid NATS NKey seed")
			}
			opts = append(opts, nats.Nkey(pub, func(nonce []byte) ([]byte, error) {
				key, err := nkeys.FromSeed([]byte(seed))
				if err != nil {
					return nil, errors.New("jawa: invalid NATS NKey seed")
				}
				defer key.Wipe()
				return key.Sign(nonce)
			}))
		}
		if c.Token != "" {
			opts = append(opts, nats.Token(c.Token))
		}
		if c.Username != "" {
			opts = append(opts, nats.UserInfo(c.Username, c.Password))
		}
		// tls:// and wss:// retain nats.go's TLS behavior and system trust.
		// For custom TLS or other auth options callers can supply cfg.Conn.
		conn, err := nats.Connect(c.URL, opts...)
		if err != nil {
			return cfg, bonnie.PresenceConfig{}, nil, errors.New("jawa: NATS presence connection failed")
		}
		c.Conn = conn
		closeConn = conn.Close
		// Conn is mutually exclusive with URL/auth in natschannel.Config.
		c.URL, c.NKeySeed, c.Token, c.Username, c.Password = "", "", "", "", ""
	}
	store, err := presencenats.New(ctx, c.Conn, presencenats.Config{
		Bucket: envOr("JAWA_NATS_PRESENCE_BUCKET", "jawa_agents"),
		TTL:    30 * time.Second, Create: c.CreateStream,
	})
	if err != nil {
		closeConn()
		return cfg, bonnie.PresenceConfig{}, nil, errors.New("jawa: could not open NATS presence bucket; check JetStream, permissions and 30s TTL")
	}
	return c, bonnie.PresenceConfig{Registry: store, AgentID: c.AgentID}, closeConn, nil
}
