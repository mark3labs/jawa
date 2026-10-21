package orchestrator

import (
	"context"
	"errors"
)

// SetArchived changes visibility without changing the card's lane or history.
func (w *Workflow) SetArchived(ctx context.Context, id string, archived bool) error {
	n, err := w.s.setCardArchived(ctx, id, archived)
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("card not found")
	}
	return nil
}
