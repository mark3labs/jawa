package reportbridge

import (
	"context"
	"encoding/json"
	"errors"
	"jawa/internal/reportprotocol"
)

// MissingReport reports whether a trusted workflow run is about to finish
// without a typed outcome. Runs without trusted NATS admission, conversational
// follow-ups, runs with an unconfirmed report and runs with an accepted
// terminal report do not qualify. An accepted proposal also does not: an
// approved planning run ends naturally. The answer only steers the model;
// the orchestrator still decides from typed reports alone.
func (b *Bridge) MissingReport(ctx context.Context, runID string) (bool, error) {
	b.mu.RLock()
	r := b.runner
	b.mu.RUnlock()
	if r == nil {
		return false, nil
	}
	records, err := r.Journal().Replay(ctx, runID)
	if err != nil {
		return false, err
	}
	if chatFollowup(records) {
		return false, nil
	}
	if _, err = b.admission(ctx, runID); err != nil {
		return false, nil
	}
	kinds := map[string]reportprotocol.Kind{}
	acks := map[string]reportprotocol.Ack{}
	for _, rec := range records {
		switch rec.ExtType {
		case intentType:
			var req reportprotocol.Request
			if json.Unmarshal(rec.Payload, &req) != nil {
				return false, errors.New("invalid durable typed report intent")
			}
			kinds[req.ReportID] = req.Kind
		case receiptType:
			var ack reportprotocol.Ack
			if json.Unmarshal(rec.Payload, &ack) != nil {
				return false, errors.New("invalid durable typed report receipt")
			}
			acks[ack.ReportID] = ack
		}
	}
	for id, kind := range kinds {
		ack, ok := acks[id]
		if !ok || ack.Terminal(kind) || (ack.Accepted && kind == reportprotocol.ProposeSpec) {
			return false, nil
		}
	}
	return true, nil
}
