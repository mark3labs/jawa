package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"jawa/internal/runapitoken"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAPIKeyWatchAuthentication(t *testing.T) {
	ns, err := server.NewServer(&server.Options{Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("not ready")
	}
	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := nc.JetStream()
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: runapitoken.Bucket})
	if err != nil {
		t.Fatal(err)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	if _, err = kv.Put(runapitoken.PublicKey, pub); err != nil {
		t.Fatal(err)
	}
	auth, stop, err := watchAPIKey(t.Context(), nc, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	request := func(token string) bool {
		r := httptest.NewRequest("GET", "/", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		p, e := auth(r)
		return e == nil && p != nil && p.ID == "orchestrator"
	}
	token := runapitoken.Sign(key, "agent-1", time.Now())
	wait := func(f func() bool) {
		t.Helper()
		for range 100 {
			if f() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("key watch did not converge")
	}
	wait(func() bool { return request(token) })
	for _, tok := range []string{"", "invalid", runapitoken.Sign(key, "other", time.Now()), runapitoken.Sign(key, "agent-1", time.Now().Add(-6*time.Minute))} {
		if request(tok) {
			t.Fatal("invalid credential accepted")
		}
	}
	nextPub, nextKey, _ := ed25519.GenerateKey(rand.Reader)
	_, err = kv.Put(runapitoken.PublicKey, nextPub)
	if err != nil {
		t.Fatal(err)
	}
	next := runapitoken.Sign(nextKey, "agent-1", time.Now())
	wait(func() bool { return request(next) && !request(token) })
	if err = kv.Delete(runapitoken.PublicKey); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return !request(next) })
}
