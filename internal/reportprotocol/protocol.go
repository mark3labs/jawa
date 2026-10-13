// Package reportprotocol defines the shared, versioned typed-report wire contract.
// A transport ACK is not a commit: Accepted is sent only after the receiver's
// report ledger and workflow transition commit in one database transaction.
package reportprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const Version = 1

type Kind string

const (
	Clarify       Kind = "clarify"
	ProposeSpec   Kind = "propose_spec"
	CompleteTask  Kind = "complete_task"
	ReportBlocker Kind = "report_blocker"
)

type Identity struct {
	TaskID     string `json:"task_id"`
	AgentID    string `json:"agent_id"`
	RunID      string `json:"run_id"`
	AttemptID  string `json:"attempt_id"`
	TurnID     string `json:"turn_id"`
	ToolCallID string `json:"tool_call_id"`
}
type Payload struct {
	Spec       string   `json:"spec,omitempty"`
	Questions  []string `json:"questions,omitempty"`
	RepoCommit string   `json:"repo_commit,omitempty"`
	PRNumber   int      `json:"pr_number,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}
type Request struct {
	Version  int    `json:"version"`
	ReportID string `json:"report_id"`
	Identity
	Kind    Kind    `json:"kind"`
	Payload Payload `json:"payload"`
}
type Ack struct {
	Version   int    `json:"version"`
	ReportID  string `json:"report_id"`
	Accepted  bool   `json:"accepted"`
	Duplicate bool   `json:"duplicate"`
	// AwaitGuidance is the receiver's durable decision that an accepted
	// reportBlocker parks the run for operator guidance instead of ending it.
	// Absent (older receivers) means the blocker is terminal.
	AwaitGuidance bool   `json:"await_guidance,omitempty"`
	Error         string `json:"error,omitempty"`
}

// Suspends reports whether an accepted report parks its run until an operator
// answers in the same conversation.
func (a Ack) Suspends(kind Kind) bool {
	return a.Accepted && (kind == Clarify || kind == ProposeSpec || (kind == ReportBlocker && a.AwaitGuidance))
}

// Terminal reports whether an accepted report ends its task's execution.
func (a Ack) Terminal(kind Kind) bool {
	return a.Accepted && (kind == CompleteTask || (kind == ReportBlocker && !a.AwaitGuidance))
}

func Subject(root string) string { return root + ".reports.submit" }

var commit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func normalize(kind Kind, p Payload) (Payload, error) {
	p.Spec = strings.TrimSpace(p.Spec)
	p.RepoCommit = strings.ToLower(strings.TrimSpace(p.RepoCommit))
	p.Reason = strings.TrimSpace(p.Reason)
	p.Questions = append([]string(nil), p.Questions...)
	for i, q := range p.Questions {
		p.Questions[i] = strings.TrimSpace(q)
	}
	switch kind {
	case Clarify:
		if len(p.Questions) == 0 || len(p.Questions) > 30 || p.Spec != "" || p.RepoCommit != "" || p.PRNumber != 0 || p.Reason != "" {
			return p, errors.New("clarify requires nonempty questions and no proposal or build fields")
		}
		for _, q := range p.Questions {
			if q == "" || len(q) > 4000 {
				return p, errors.New("invalid clarification question")
			}
		}
	case ProposeSpec:
		if p.Spec == "" || len(p.Spec) > 256*1024 || !commit.MatchString(p.RepoCommit) || p.Questions != nil || p.PRNumber != 0 || p.Reason != "" {
			return p, errors.New("proposeSpec requires a specification and full inspected commit SHA, and no questions or build fields")
		}
	case CompleteTask:
		if p.PRNumber < 1 || p.Spec != "" || len(p.Questions) != 0 || p.RepoCommit != "" || p.Reason != "" {
			return p, errors.New("completeTask requires a positive PR number and no planning fields")
		}
	case ReportBlocker:
		if p.Reason == "" || len(p.Reason) > 16000 || p.Spec != "" || len(p.Questions) != 0 || p.RepoCommit != "" || p.PRNumber != 0 {
			return p, errors.New("reportBlocker requires a nonempty reason and no completion fields")
		}
	default:
		return p, errors.New("unknown report kind")
	}
	return p, nil
}

// NewRequest hashes the normalized task/report payload, scoped to a trusted tool
// interaction. Transport retries preserve the original request identity. Receivers must separately enforce owner.
func NewRequest(id Identity, kind Kind, p Payload) (Request, error) {
	for _, v := range []string{id.TaskID, id.AgentID, id.RunID, id.AttemptID, id.TurnID, id.ToolCallID} {
		if v == "" || len(v) > 256 || strings.ContainsAny(v, "\x00\r\n") {
			return Request{}, errors.New("missing or invalid trusted report identity")
		}
	}
	p, err := normalize(kind, p)
	if err != nil {
		return Request{}, err
	}
	data, err := json.Marshal(struct {
		TaskID     string  `json:"task_id"`
		ToolCallID string  `json:"tool_call_id"`
		Kind       Kind    `json:"kind"`
		Payload    Payload `json:"payload"`
	}{id.TaskID, id.ToolCallID, kind, p})
	if err != nil {
		return Request{}, err
	}
	hash := sha256.Sum256(data)
	return Request{Version: Version, ReportID: hex.EncodeToString(hash[:]), Identity: id, Kind: kind, Payload: p}, nil
}
func (r Request) Validate() error {
	if r.Version != Version {
		return errors.New("unsupported typed report version")
	}
	canonical, err := NewRequest(r.Identity, r.Kind, r.Payload)
	if err != nil {
		return err
	}
	if r.ReportID != canonical.ReportID {
		return fmt.Errorf("typed report hash mismatch")
	}
	// Require canonical wire payload, preventing differing stored bytes for one ID.
	a, _ := json.Marshal(r.Payload)
	b, _ := json.Marshal(canonical.Payload)
	if string(a) != string(b) {
		return errors.New("typed report payload is not canonical")
	}
	return nil
}
