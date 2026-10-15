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

func (s *Store) setCardArchived(ctx context.Context, id string, archived bool) (int64, error) {
	var flag int64
	if archived {
		flag = 1
	}
	tx, err := s.writeTx()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = noUnresolvedMerge(tx, id); err != nil {
		return 0, err
	}
	n, err := sqlc.New(tx).SetCardArchived(ctx, sqlc.SetCardArchivedParams{ID: id, Archived: flag})
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func cardFromQuery(row sqlc.Card) Card {
	return Card{AutoMergeMode: row.AutoMergeMode, ID: row.ID, ProjectID: row.ProjectID, Title: row.Title,
		Description: row.Description, Status: row.Status, Position: int(row.Position),
		Archived:  row.Archived != 0,
		CreatedAt: time.Unix(row.CreatedAt, 0).UTC(), IssueURL: row.IssueUrl}
}
