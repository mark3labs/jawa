package orchestrator

import (
	"context"
	"time"

	"jawa/internal/orchestrator/db/sqlc"
)

// queryCards adapts generated query results to domain cards for Store.Cards.
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

func cardFromQuery(row sqlc.Card) Card {
	return Card{ID: row.ID, ProjectID: row.ProjectID, Title: row.Title,
		Description: row.Description, Status: row.Status, Position: int(row.Position),
		CreatedAt: time.Unix(row.CreatedAt, 0).UTC(), IssueURL: row.IssueUrl}
}
