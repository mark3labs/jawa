package reportbridge

import (
	"encoding/json"
	"errors"

	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/reportprotocol"
)

const unblockIntentType = "jawa_unblock_authorization_v1"

// Reopening only suppresses an accepted blocker in the exact authorized old
// generation. Unresolved intents and completions are never superseded.
func supersededBlockers(records []runtime.Record, turn string) (map[string]bool, error) {

	superseded := map[string]bool{}
	var authorization *reportprotocol.FailedResume
	accepted := map[string]bool{}
	bound := ""
	for _, rec := range records {
		switch rec.ExtType {
		case receiptType:
			var ack reportprotocol.Ack
			if json.Unmarshal(rec.Payload, &ack) == nil {
				accepted[ack.ReportID] = ack.Accepted
			}
		case unblockIntentType:
			var req reportprotocol.FailedResume
			if json.Unmarshal(rec.Payload, &req) != nil || req.Mode != "unblock" || req.Generation < 1 {
				return nil, errors.New("invalid unblock authorization")
			}
			authorization = &req
			bound = ""
		case resumeReceiptType:
			var receipt reportprotocol.FailedResumeReply
			if json.Unmarshal(rec.Payload, &receipt) == nil && authorization != nil && receipt.ID == authorization.ID && receipt.Status != "completed" && receipt.Status != "pending" && bound == "" {
				authorization = nil
			}
		}
		if rec.Kind == runtime.RecordTurn && authorization != nil && rec.Seq > authorization.Seq && rec.Text != authorization.TurnID {
			bound = rec.Text
			if authorization.BlockerReportID != "" {
				if !accepted[authorization.BlockerReportID] {
					return nil, errors.New("unblock cannot supersede an unconfirmed report")
				}
				superseded[authorization.BlockerReportID] = true
			}
		}
	}
	if authorization != nil && turn != "" && (bound == "" || bound != turn) {
		return nil, errors.New("report does not match authorized continuation turn")
	}

	return superseded, nil
}

func validateUnblockReports(records []runtime.Record, req reportprotocol.FailedResume) error {
	superseded, err := supersededBlockers(records, "")
	if err != nil {
		return err
	}
	intents := map[string]reportprotocol.Request{}
	receipts := map[string]reportprotocol.Ack{}
	generation := 0
	for _, rec := range records {
		switch rec.ExtType {
		case unblockIntentType:
			var old reportprotocol.FailedResume
			if json.Unmarshal(rec.Payload, &old) == nil && old.Generation > generation {
				generation = old.Generation
			}
		case intentType:
			var old reportprotocol.Request
			if json.Unmarshal(rec.Payload, &old) != nil {
				return errors.New("invalid report intent")
			}
			intents[old.ReportID] = old
		case receiptType:
			var ack reportprotocol.Ack
			if json.Unmarshal(rec.Payload, &ack) != nil {
				return errors.New("invalid report receipt")
			}
			receipts[ack.ReportID] = ack
		}
	}
	// Failed/fenced authorizations can be retried at the same generation.
	if req.Generation < generation || req.Generation > generation+1 {
		return errors.New("unblock generation changed")
	}
	found := req.BlockerReportID == ""
	for id, old := range intents {
		ack, ok := receipts[id]
		if !ok {
			return errors.New("unresolved report intent prevents unblock")
		}
		if !ack.Accepted || (superseded[id] && id != req.BlockerReportID) {
			continue
		}
		if old.Kind == reportprotocol.CompleteTask {
			return errors.New("accepted completion prevents unblock")
		}
		if old.Kind == reportprotocol.ReportBlocker {
			if id != req.BlockerReportID || old.TurnID != req.TurnID || old.TaskID != req.Target.TaskID || old.AgentID != req.Target.AgentID || old.AttemptID != req.Target.AttemptID {
				return errors.New("unblock blocker identity changed")
			}
			found = true
		}
	}
	if !found {
		return errors.New("accepted blocker receipt unavailable")
	}
	return nil
}
