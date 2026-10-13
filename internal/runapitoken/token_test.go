package runapitoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(10000, 0)
	tok := Sign(key, "agent-1", now)
	if err := Verify(pub, tok, "agent-1", now); err != nil {
		t.Fatal(err)
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	for name, f := range map[string]func() error{
		"audience":    func() error { return Verify(pub, tok, "agent-2", now) },
		"expired":     func() error { return Verify(pub, tok, "agent-1", now.Add(Lifetime)) },
		"future":      func() error { return Verify(pub, tok, "agent-1", now.Add(-31*time.Second)) },
		"rotated":     func() error { return Verify(other, tok, "agent-1", now) },
		"missing key": func() error { return Verify(nil, tok, "agent-1", now) },
		"trailing":    func() error { return Verify(pub, tok+".x", "agent-1", now) },
	} {
		t.Run(name, func(t *testing.T) {
			if f() == nil {
				t.Fatal("accepted invalid token")
			}
		})
	}
}
func TestSubject(t *testing.T) {
	for _, id := range []string{"", "a.b", "*", "a b"} {
		if _, err := Subject("bonnie", id); err == nil {
			t.Fatal(id)
		}
	}
}
