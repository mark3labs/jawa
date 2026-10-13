// Package runapitoken authenticates trusted orchestrator requests to agents.
package runapitoken

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

const Bucket = "jawa_control"
const PublicKey = "api_public_key"
const Lifetime = 5 * time.Minute

var safe = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)
var rootSafe = regexp.MustCompile(`^[a-zA-Z0-9_-]+(\.[a-zA-Z0-9_-]+)*$`)
var ErrInvalid = errors.New("invalid API credential")

type Claims struct {
	Aud string `json:"aud"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
}

func Subject(root, agent string) (string, error) {
	if root == "" {
		root = "bonnie"
	}
	if !rootSafe.MatchString(root) || !safe.MatchString(agent) {
		return "", ErrInvalid
	}
	return root + ".api.agent." + agent, nil
}
func Sign(key ed25519.PrivateKey, agent string, now time.Time) string {
	data, _ := json.Marshal(Claims{agent, now.Unix(), now.Add(Lifetime).Unix()})
	claims := base64.RawURLEncoding.EncodeToString(data)
	return claims + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(claims)))
}
func Verify(key ed25519.PublicKey, token, agent string, now time.Time) error {
	if len(key) != ed25519.PublicKeySize || len(token) > 2048 {
		return ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return ErrInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]), sig) {
		return ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalid
	}
	var c Claims
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	if c.Aud != agent || c.Exp <= now.Unix() || c.Iat > now.Add(30*time.Second).Unix() || c.Exp <= c.Iat || c.Exp-c.Iat > int64(Lifetime/time.Second) {
		return ErrInvalid
	}
	return nil
}
