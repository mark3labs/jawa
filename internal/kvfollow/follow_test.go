package kvfollow

import (
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

type recorder struct {
	mu      sync.Mutex
	value   string
	present bool
	trusted bool
	lost    int
}

func (r *recorder) config(bucket string) Config {
	return Config{Bucket: bucket, Key: "k",
		Apply: func(v []byte, present bool) {
			r.mu.Lock()
			r.value, r.present, r.trusted = string(v), present, true
			r.mu.Unlock()
		},
		Lost: func() {
			r.mu.Lock()
			r.trusted = false
			r.lost++
			r.mu.Unlock()
		}}
}

func (r *recorder) waitFor(t *testing.T, what string, ok func(value string, present, trusted bool) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		done := ok(r.value, r.present, r.trusted)
		r.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("follower did not reach %s", what)
}

func natsServer(t *testing.T, dir string, port int) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{Port: port, JetStream: true, StoreDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS not ready")
	}
	return ns
}

func TestFollowValueBucketLifecycleAndReconnect(t *testing.T) {
	dir := t.TempDir()
	ns := natsServer(t, dir, -1)
	url := ns.ClientURL()
	nc, err := nats.Connect(url, nats.MaxReconnects(-1), nats.ReconnectWait(20*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	var r recorder
	// The bucket does not exist yet: absence is applied and creation is awaited.
	stop, err := Follow(t.Context(), nc, r.config("follow"))
	if err == nil {
		t.Fatal("missing bucket reported success")
	}
	defer stop()
	r.waitFor(t, "absent", func(_ string, present, trusted bool) bool { return !present && trusted })
	js, _ := nc.JetStream()
	kv, err := js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "follow"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = kv.Put("k", []byte("one")); err != nil {
		t.Fatal(err)
	}
	// Creation is announced, so this converges well before any backoff.
	start := time.Now()
	r.waitFor(t, "first value", func(v string, present, _ bool) bool { return present && v == "one" })
	if time.Since(start) > 900*time.Millisecond {
		t.Fatalf("bucket creation waited for backoff: %v", time.Since(start))
	}
	if _, err = kv.Put("k", []byte("two")); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "update", func(v string, _, _ bool) bool { return v == "two" })
	if err = kv.Delete("k"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "deleted key", func(_ string, present, _ bool) bool { return !present })
	if _, err = kv.Put("k", []byte("three")); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "restored key", func(v string, present, _ bool) bool { return present && v == "three" })
	// Deleting the bucket revokes the value at once.
	if err = js.DeleteKeyValue("follow"); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "deleted bucket", func(_ string, present, _ bool) bool { return !present })
	if kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: "follow"}); err != nil {
		t.Fatal(err)
	}
	if _, err = kv.Put("k", []byte("four")); err != nil {
		t.Fatal(err)
	}
	r.waitFor(t, "recreated bucket", func(v string, present, _ bool) bool { return present && v == "four" })
	// A server restart marks the value lost, then a reconnect re-reads it.
	addr := ns.Addr().String()
	ns.Shutdown()
	ns.WaitForShutdown()
	r.waitFor(t, "lost on disconnect", func(_ string, _, trusted bool) bool { return !trusted })
	_, portText, _ := splitHostPort(addr)
	restarted := natsServer(t, dir, portText)
	defer restarted.Shutdown()
	r.waitFor(t, "value after reconnect", func(v string, present, trusted bool) bool { return trusted && present && v == "four" })
}

func splitHostPort(addr string) (string, int, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			port := 0
			for _, c := range addr[i+1:] {
				port = port*10 + int(c-'0')
			}
			return addr[:i], port, nil
		}
	}
	return addr, 0, nil
}
