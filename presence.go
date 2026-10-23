package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/mark3labs/bonnie"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	natsapi "github.com/mark3labs/bonnie/channel/natsapi"
	presencenats "github.com/mark3labs/bonnie/presence/nats"
	"github.com/mark3labs/bonnie/runtime"
	kit "github.com/mark3labs/kit/pkg/kit"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	"github.com/spf13/cobra"
	"jawa/internal/runapitoken"
)

// JetStream KV agent discovery is required. All agents
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
	// Providers are installed after parsing so workspace/image flags take effect.
	if flag := cmd.Flags().Lookup("sandbox"); flag != nil {
		flag.Usage = "sandbox backend (local, microsandbox); default local"
	}
	apiEnabled := envOr("JAWA_NATS_API", "true") == "true"
	bucket := envOr("JAWA_NATS_PRESENCE_BUCKET", defaultPresenceBucket)
	root := envOr("JAWA_SANDBOX_ROOT", defaultSandboxRoot)
	image := envOr("JAWA_SANDBOX_IMAGE", defaultSandboxImage)
	token := os.Getenv("GITHUB_TOKEN")
	forgejo := forgejoToken()
	apiKey, providerURL := "", ""
	decisionModel := os.Getenv("JAWA_DECISION_MODEL")
	cmd.Flags().BoolVar(&apiEnabled, "nats-api", apiEnabled, "serve authenticated run API over NATS (JAWA_NATS_API)")
	cmd.Flags().StringVar(&bucket, "nats-presence-bucket", bucket, "discovery bucket (JAWA_NATS_PRESENCE_BUCKET)")
	cmd.Flags().StringVar(&root, "sandbox-root", root, "local workspace root (JAWA_SANDBOX_ROOT)")
	cmd.Flags().StringVar(&image, "sandbox-image", image, "microsandbox guest image (JAWA_SANDBOX_IMAGE)")
	cmd.Flags().StringVar(&token, "github-token", token, "GitHub credential for coding commands (GITHUB_TOKEN); prefer environment")
	cmd.Flags().Lookup("github-token").DefValue = ""
	cmd.Flags().StringVar(&forgejo, "forgejo-token", forgejo, "Forgejo credential for coding commands (JAWA_FORGEJO_TOKEN or FORGEJO_TOKEN); prefer environment")
	cmd.Flags().Lookup("forgejo-token").DefValue = ""
	cmd.Flags().StringVar(&apiKey, "provider-api-key", "", "override model provider credential; prefer provider environment variables")
	cmd.Flags().StringVar(&providerURL, "provider-url", "", "override model provider endpoint")
	cmd.Flags().StringVar(&decisionModel, "decision-model", decisionModel, "optional decision model in provider/model format (JAWA_DECISION_MODEL); empty disables decisions")
	servingFlagsE(cmd, func(name string, cfg natschannel.Config) error {
		model, provider, modelID, err := servingModel(cmd)
		if err != nil {
			return err
		}
		if err := validDecisionModel(decisionModel); err != nil {
			return err
		}
		if decisionModel != "" {
			agent.Configure(bonnie.WithDecisionModel(decisionModel))
		}
		env := codingEnv()
		delete(env, "GITHUB_TOKEN")
		if token != "" {
			env["GITHUB_TOKEN"] = token
		}
		delete(env, "FORGEJO_TOKEN")
		if forgejo != "" {
			env["FORGEJO_TOKEN"] = forgejo
		}
		agent.Configure(bonnie.WithModel(model), bonnie.WithSandboxes(codingSandboxesWith(root, image)...), bonnie.WithSandboxEnv(env), bonnie.WithKit(func(o *kit.Options) {
			o.ProviderAPIKey = apiKey
			o.ProviderURL = providerURL
		}))
		shared, pc, closeConn, err := openNATSPresenceWithBucket(cmd.Context(), cfg, bucket)
		if err != nil {
			return err
		}
		pc.Labels = map[string]string{"jawa.provider": provider, "jawa.model": modelID}
		cleanup = closeConn
		if apiEnabled {
			subject, err := runapitoken.Subject(shared.RootSubject, shared.AgentID)
			if err != nil {
				closeConn()
				return err
			}
			auth, stop, err := watchAPIKey(cmd.Context(), shared.Conn, shared.AgentID)
			if err != nil {
				closeConn()
				return err
			}
			cleanup = func() { stop(); closeConn() }
			pc.Labels["jawa.api_subject"] = subject
			pc.Labels["jawa.run_chat"] = "1"
			agent.Configure(bonnie.WithNATSAPI(natsapi.Config{Conn: shared.Conn, Subject: subject, Authenticator: auth}))
		}
		agent.Configure(bonnie.WithName(name), trustedReports.Channel(shared), bonnie.WithPresence(pc))
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
	return openNATSPresenceWithBucket(ctx, natsAuthConfig(cfg), envOr("JAWA_NATS_PRESENCE_BUCKET", defaultPresenceBucket))
}

func openNATSPresenceWithBucket(ctx context.Context, cfg natschannel.Config, bucket string) (natschannel.Config, bonnie.PresenceConfig, func(), error) {
	c := cfg
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
		Bucket: bucket,
		TTL:    30 * time.Second, Create: c.CreateStream,
	})
	if err != nil {
		closeConn()
		return cfg, bonnie.PresenceConfig{}, nil, errors.New("jawa: could not open NATS presence bucket; check JetStream, permissions and 30s TTL")
	}
	return c, bonnie.PresenceConfig{Registry: store, AgentID: c.AgentID}, closeConn, nil
}

// An empty selection disables decisions. BONNIE checks that Kit can call the
// model at startup; credentials are only checked by the first call.
func validDecisionModel(selection string) error {
	if selection == "" {
		return nil
	}
	if _, _, err := kit.ParseModelString(selection); err != nil {
		return errors.New("jawa: invalid decision model selection; expected provider/model")
	}
	return nil
}

// Match BONNIE's nonempty --model override, including explicit-empty semantics.
// Resolve before opening network resources; parser diagnostics may echo secrets.
func servingModel(cmd *cobra.Command) (selection, provider, modelID string, err error) {
	selection = startupModel()
	if flag := cmd.Flags().Lookup("model"); flag != nil && flag.Value.String() != "" {
		selection = flag.Value.String()
	}
	provider, modelID, err = kit.ParseModelString(selection)
	if err != nil {
		return "", "", "", errors.New("jawa: invalid model selection; expected provider/model")
	}
	return selection, provider, modelID, nil
}
