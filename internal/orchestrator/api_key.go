package orchestrator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/nats-io/nats.go"
	"jawa/internal/runapitoken"
	"time"
)

// The seed is private settings data, never a dashboard override.
func (s *Store) apiSigningKey() (ed25519.PrivateKey, error) {
	tx, err := s.writeTx()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var value string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key='api_signing_key'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err = rand.Read(seed); err != nil {
			return nil, err
		}
		value = base64.StdEncoding.EncodeToString(seed)
		_, err = tx.Exec(`INSERT INTO settings(key,value) VALUES('api_signing_key',?)`, value)
	}
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("invalid stored API signing key")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return ed25519.NewKeyFromSeed(seed), nil
}
func publishAPIKey(ctx context.Context, nc *nats.Conn, key ed25519.PrivateKey) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	js, err := nc.JetStream(nats.Context(ctx))
	if err != nil {
		return err
	}
	kv, err := js.KeyValue(runapitoken.Bucket)
	if errors.Is(err, nats.ErrBucketNotFound) {
		kv, err = js.CreateKeyValue(&nats.KeyValueConfig{Bucket: runapitoken.Bucket, History: 1, Storage: nats.FileStorage})
	}
	if err != nil {
		return err
	}
	pub := key.Public().(ed25519.PublicKey)
	old, err := kv.Get(runapitoken.PublicKey)
	if err == nil && bytes.Equal(old.Value(), pub) {
		return nil
	}
	if err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
		return err
	}
	_, err = kv.Put(runapitoken.PublicKey, pub)
	return err
}
