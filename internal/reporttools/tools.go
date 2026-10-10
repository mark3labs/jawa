// Package reporttools implements model-visible reports with host-bound identity.
package reporttools

import (
	"context"
	kit "github.com/mark3labs/kit/pkg/kit"
	"jawa/internal/reportprotocol"
)

type Binding struct {
	Identity func(context.Context) (reportprotocol.Identity, error)
	Submit   func(context.Context, reportprotocol.Request) (reportprotocol.Ack, error)
}
type SpecInput struct {
	Spec       string   `json:"spec" description:"Implementation-ready specification based on repository inspection"`
	Questions  []string `json:"questions" description:"Clarification questions; empty when ready"`
	RepoCommit string   `json:"repo_commit" description:"Full SHA of inspected repository commit"`
}
type CompleteInput struct {
	PRNumber int `json:"pr_number" description:"Positive pull request number opened for this task"`
}
type BlockerInput struct {
	Reason string `json:"reason" description:"Concrete reason this task cannot proceed"`
}

func (b Binding) execute(ctx context.Context, kind reportprotocol.Kind, p reportprotocol.Payload) (kit.ToolOutput, error) {
	if b.Identity == nil || b.Submit == nil {
		return kit.ErrorResult("typed report tools require a trusted managed NATS task; no identity may be supplied by the model"), nil
	}
	id, err := b.Identity(ctx)
	if err != nil {
		return kit.ErrorResult(err.Error()), nil
	}
	r, err := reportprotocol.NewRequest(id, kind, p)
	if err != nil {
		return kit.ErrorResult(err.Error()), nil
	}
	ack, err := b.Submit(ctx, r)
	if err != nil {
		return kit.ErrorResult(err.Error()), nil
	}
	if !ack.Accepted || ack.Error != "" || ack.Version != reportprotocol.Version || ack.ReportID != r.ReportID {
		return kit.ErrorResult("typed report commit acknowledgement does not match report"), nil
	}
	return kit.ToolOutput{Content: "Typed report committed.", Halt: true, FinalValue: r, Metadata: ack}, nil
}
func ProposeSpec(b Binding) kit.Tool {
	return kit.NewTool("proposeSpec", "Submit a terminal planning specification and questions. Planning tasks only. Success means durable orchestrator commit and ends this run.", func(ctx context.Context, in SpecInput) (kit.ToolOutput, error) {
		return b.execute(ctx, reportprotocol.ProposeSpec, reportprotocol.Payload{Spec: in.Spec, Questions: in.Questions, RepoCommit: in.RepoCommit})
	})
}
func CompleteTask(b Binding) kit.Tool {
	return kit.NewTool("completeTask", "Report the opened PR for a build task. Build tasks only. Does not grant review readiness. Success means durable orchestrator commit and ends this run.", func(ctx context.Context, in CompleteInput) (kit.ToolOutput, error) {
		return b.execute(ctx, reportprotocol.CompleteTask, reportprotocol.Payload{PRNumber: in.PRNumber})
	})
}
func ReportBlocker(b Binding) kit.Tool {
	return kit.NewTool("reportBlocker", "Report a terminal blocker for the current planning or build task. Success means durable orchestrator commit and ends this run.", func(ctx context.Context, in BlockerInput) (kit.ToolOutput, error) {
		return b.execute(ctx, reportprotocol.ReportBlocker, reportprotocol.Payload{Reason: in.Reason})
	})
}
func Tools(b Binding) []kit.Tool {
	return []kit.Tool{ProposeSpec(b), CompleteTask(b), ReportBlocker(b)}
}
