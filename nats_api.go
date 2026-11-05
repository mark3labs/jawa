package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/mark3labs/bonnie/channel"
	bonniehttp "github.com/mark3labs/bonnie/channel/http"
	"github.com/nats-io/nats.go"
	"jawa/internal/kvfollow"
	"jawa/internal/runapitoken"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// The orchestrator's public key is trusted only while it is being followed:
// a disconnect, a failed watch, a deleted or purged key and a deleted bucket
// all clear it at once (fail closed), and a reconnect re-reads it. Nothing is
// re-read on a timer. Shutdown joins the follower before the caller closes its
// shared connection.
func watchAPIKey(ctx context.Context, nc *nats.Conn, agent string) (bonniehttp.Authenticator, func(), error) {
	var key atomic.Pointer[ed25519.PublicKey]
	stop, err := kvfollow.Follow(ctx, nc, kvfollow.Config{
		Bucket: runapitoken.Bucket,
		Key:    runapitoken.PublicKey,
		Apply: func(value []byte, present bool) {
			if !present || len(value) != ed25519.PublicKeySize {
				key.Store(nil)
				return
			}
			pub := ed25519.PublicKey(append([]byte(nil), value...))
			key.Store(&pub)
		},
		Lost: func() { key.Store(nil) },
	})
	if err != nil {
		stop()
		return nil, nil, errors.New("jawa: NATS API needs the orchestrator control bucket; upgrade orchestrator or disable --nats-api")
	}
	auth := func(r *http.Request) (*channel.Principal, error) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		pub := key.Load()
		if !ok || pub == nil || runapitoken.Verify(*pub, token, agent, time.Now()) != nil {
			return nil, bonniehttp.ErrUnauthenticated
		}
		return &channel.Principal{Authenticator: "jawa-orchestrator", Kind: "user", ID: "orchestrator"}, nil
	}
	return auth, stop, nil
}
