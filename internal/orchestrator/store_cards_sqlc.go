package orchestrator

import (
	"context"
	"time"

	"jawa/internal/orchestrator/db/sqlc"
)

// queryCards is the generated read slice, kept separate so card/status work can
// switch Cards to this helper without concurrent edits to store.go business logic.
func (s *Store) queryCards(ctx context.Context, project string) ([]Card, error) {
	rows, err := sqlc.New(s.db).ListCards(ctx, project)
	if err != nil {
		return nil, err
	}
	cards := make([]Card, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, cardFromQuery(row))
	}
	return cards, nil
}

// queryCard is available to planning operations; use sqlc.New(tx) for operations
// that must share a writeTx transaction with lane updates or session creation.
func (s *Store) queryCard(ctx context.Context, id string) (Card, error) {
	row, err := sqlc.New(s.db).GetCard(ctx, id)
	if err != nil {
		return Card{}, err
	}
	return cardFromQuery(row), nil
}

func cardFromQuery(row sqlc.Card) Card {
	return Card{ID: row.ID, ProjectID: row.ProjectID, Title: row.Title,
		Description: row.Description, Status: row.Status, Position: int(row.Position),
		CreatedAt: time.Unix(row.CreatedAt, 0).UTC(), IssueURL: row.IssueUrl}
}
