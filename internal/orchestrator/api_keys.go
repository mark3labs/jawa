package orchestrator

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"
	"jawa/internal/orchestrator/db/sqlc"
)

// APIKey never carries the secret or its hash.
type APIKey struct {
	ID, Name, Prefix                 string
	CreatedAt, LastUsedAt, RevokedAt time.Time
}

const apiKeyPrefix = "jawa_"

var errAPIKeyInvalid = errors.New("invalid API key")

// CreateAPIKey returns the only copy of the secret. Callers must show it once
// and never log or persist it.
func (s *Store) CreateAPIKey(ctx context.Context, name string) (APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 64 || strings.ContainsRune(name, 0) {
		return APIKey{}, "", errors.New("key name must be 1–64 characters")
	}
	secret := apiKeyPrefix + newID()
	k := APIKey{ID: newOperationID(), Name: name, Prefix: secret[:len(apiKeyPrefix)+8], CreatedAt: time.Now().UTC().Truncate(time.Second)}
	err := sqlc.New(s.db).InsertAPIKey(ctx, sqlc.InsertAPIKeyParams{ID: k.ID, Name: k.Name, TokenHash: tokenHash(secret), Prefix: k.Prefix, CreatedAt: k.CreatedAt.Unix()})
	return k, secret, err
}

func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := sqlc.New(s.db).ListAPIKeys(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]APIKey, 0, len(rows))
	for _, row := range rows {
		k := APIKey{ID: row.ID, Name: row.Name, Prefix: row.Prefix, CreatedAt: time.Unix(row.CreatedAt, 0).UTC()}
		if row.LastUsedAt != 0 {
			k.LastUsedAt = time.Unix(row.LastUsedAt, 0).UTC()
		}
		if row.RevokedAt != 0 {
			k.RevokedAt = time.Unix(row.RevokedAt, 0).UTC()
		}
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string) error {
	n, err := sqlc.New(s.db).RevokeAPIKey(ctx, sqlc.RevokeAPIKeyParams{Now: time.Now().Unix(), ID: id})
	if err == nil && n == 0 {
		err = errors.New("API key not found or already revoked")
	}
	return err
}

// AuthenticateAPIKey uses the hash of a 256-bit secret; hash equality leaks
// nothing useful and does not require a constant-time comparison.
func (s *Store) AuthenticateAPIKey(ctx context.Context, secret string, now time.Time) (APIKey, error) {
	if !strings.HasPrefix(secret, apiKeyPrefix) || len(secret) != len(apiKeyPrefix)+64 {
		return APIKey{}, errAPIKeyInvalid
	}
	q := sqlc.New(s.db)
	row, err := q.GetActiveAPIKeyByHash(ctx, tokenHash(secret))
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, errAPIKeyInvalid
	}
	if err != nil {
		return APIKey{}, err
	}
	// Coarse last-used tracking: at most one write per key per minute.
	_ = q.TouchAPIKey(ctx, sqlc.TouchAPIKeyParams{Now: now.Unix(), ID: row.ID, Before: now.Add(-time.Minute).Unix()})
	return APIKey{ID: row.ID, Name: row.Name}, nil
}

func (a *app) mutateAPIKeyCreate(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	k, secret, err := a.s.CreateAPIKey(r.Context(), r.FormValue("name"))
	if err != nil {
		return err
	}
	if a.logger != nil {
		a.logger.Info("API key created", "api_key_id", k.ID)
	}
	ctx.handled = true
	w.Header().Set("Cache-Control", "no-store")
	render := func(c templ.Component) string { var b bytes.Buffer; _ = c.Render(r.Context(), &b); return b.String() }
	if datastarRequest(r) {
		content, err := a.mutationSnapshot(r, parseViewParams(r, ""), mutationEffects{})
		if err != nil {
			// Creation already committed. A workspace refresh failure must not
			// discard the only copy of the new secret.
			_ = newSSEWriter(w).write(elementsFrameMode("inner", "#dialog-slot", render(apiKeyCreatedDialog(newOperationID(), k, secret))))
			return nil
		}
		stream := newSSEWriter(w)
		if stream.write(elementsFrame(content)) != nil {
			return nil
		}
		_ = stream.write(elementsFrameMode("inner", "#dialog-slot", render(apiKeyCreatedDialog(newOperationID(), k, secret))))
		return nil
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = apiKeyCreatedPage(k, secret).Render(r.Context(), w)
	return nil
}

func (a *app) mutateAPIKeyRevoke(w http.ResponseWriter, r *http.Request, ctx *mutationContext) error {
	err := a.s.RevokeAPIKey(r.Context(), r.FormValue("key_id"))
	if err == nil {
		ctx.effects.flash = "API key revoked. MCP clients using it are rejected immediately."
		if a.logger != nil {
			a.logger.Info("API key revoked", "api_key_id", r.FormValue("key_id"))
		}
	}
	return err
}
