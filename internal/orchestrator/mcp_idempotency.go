package orchestrator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"jawa/internal/orchestrator/db/sqlc"
)

var errIdempotencyConflict = errors.New("idempotency_key was already used with different arguments")
var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// mcpArgsHash fingerprints arguments; callers zero IdempotencyKey first.
func mcpArgsHash(tool string, args any) string {
	b, _ := json.Marshal(args)
	return tokenHash(tool + "\x00" + string(b))
}

// replayOrRun commits creation and its replay record in the same transaction.
func (s *Store) replayOrRun(ctx context.Context, keyID, idem, tool, argsHash string, run func(*sql.Tx) (string, error)) (result string, replayed bool, err error) {
	if !idempotencyKeyPattern.MatchString(idem) {
		return "", false, errors.New("idempotency_key must be 1–128 characters of A-Z a-z 0-9 . _ : -")
	}
	tx, err := s.writeTx()
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback() }()
	q := sqlc.New(tx)
	prev, err := q.GetMCPRequest(ctx, sqlc.GetMCPRequestParams{KeyID: keyID, IdempotencyKey: idem})
	if err == nil {
		if prev.Tool != tool || prev.ArgsHash != argsHash {
			return "", false, errIdempotencyConflict
		}
		return prev.Result, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	if result, err = run(tx); err != nil {
		return "", false, err
	}
	if err = q.InsertMCPRequest(ctx, sqlc.InsertMCPRequestParams{KeyID: keyID, IdempotencyKey: idem, Tool: tool, ArgsHash: argsHash, Result: result, CreatedAt: time.Now().Unix()}); err != nil {
		return "", false, err
	}
	return result, false, tx.Commit()
}
