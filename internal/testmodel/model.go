// Package testmodel supplies an offline scripted provider for real KIT integration tests.
// Adapted from BONNIE v0.22.0 internal/fakemodel (MIT licensed).
package testmodel

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"

	kit "github.com/mark3labs/kit/pkg/kit"
)

// Provider is the provider name [Model.Option] registers, and ModelName the
// model it selects. The model string is Provider + "/" + ModelName.
const (
	Provider  = "fakemodel"
	ModelName = "scripted"
)

// ErrScriptExhausted is returned when Kit asks for more replies than the
// script holds. A test that sees it asked the model one more time than it
// expected, which is usually the finding.
var ErrScriptExhausted = errors.New("fakemodel: script exhausted")

// ToolCall is one tool call in a scripted reply. Input is the JSON object the
// tool receives.
type ToolCall struct {
	Name  string
	Input string
}

// Reply is what the model answers to one request: text, tool calls, or both.
// A reply with tool calls ends its step with the tool-calls finish reason, so
// Kit runs the tools and asks again; a reply without ends the turn.
type Reply struct {
	Text  string
	Calls []ToolCall
}

// Say is a reply that is text only, which ends the turn.
func Say(text string) Reply { return Reply{Text: text} }

// Call is a reply that calls one tool with a JSON input.
func Call(name, input string) Reply {
	return Reply{Calls: []ToolCall{{Name: name, Input: input}}}
}

// Request is what Kit sent to the model for one reply.
type Request struct {
	// Messages is the whole prompt, system message included, in order.
	Messages []kit.LLMMessage
	// Tools names every tool the model was offered.
	Tools []string
}

// System returns the text of the request's system messages: the system
// prompt Kit composed, with its context files, skills and environment block.
func (r Request) System() string {
	return r.text(func(m kit.LLMMessage) bool { return m.Role == kit.LLMMessageRole("system") })
}

// Text returns the text of every message in the request, one part per line.
func (r Request) Text() string {
	return r.text(func(kit.LLMMessage) bool { return true })
}

func (r Request) text(keep func(kit.LLMMessage) bool) string {
	var b strings.Builder
	for _, m := range r.Messages {
		if !keep(m) {
			continue
		}
		for _, part := range m.Content {
			if t, ok := part.(kit.LLMTextPart); ok {
				b.WriteString(t.Text)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// HasTool reports whether the request offered the named tool.
func (r Request) HasTool(name string) bool {
	return slices.Contains(r.Tools, name)
}

// Model is a scripted language model. It answers each request with the next
// reply of its script and records the request. It is safe for concurrent use.
//
// One Model may back several Kit instances in turn — a run that suspends in
// one Runner and resumes in another — and the script then continues where
// the first instance left it.
type Model struct {
	mu       sync.Mutex
	replies  []Reply
	requests []Request
	callIDs  int
}

// New returns a model that answers with replies, in order.
func New(replies ...Reply) *Model {
	return &Model{replies: replies}
}

// Option registers the model as the provider [Provider] on one Kit and
// selects it. It does not change any other option, so a test sees the
// configuration the code under test builds.
func (m *Model) Option() kit.Option {
	return func(o *kit.Options) {
		kit.WithProvider(Provider, m.factory)(o)
		kit.WithModel(Provider + "/" + ModelName)(o)
	}
}

// Requests returns a copy of every request the model received, in order.
func (m *Model) Requests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.requests...)
}

// Remaining returns how many replies of the script are not used yet.
func (m *Model) Remaining() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.replies)
}

func (m *Model) factory(context.Context, *kit.ProviderConfig, string) (*kit.ProviderResult, error) {
	return &kit.ProviderResult{Model: languageModel{m}}, nil
}

// answer records call and returns the next reply, with a call ID for each
// tool call. The IDs are unique across the model's life, because a replayed
// conversation that repeats an ID is a defect a provider would reject.
func (m *Model) answer(call fantasy.Call) (Reply, []string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	req := Request{Messages: append([]kit.LLMMessage(nil), call.Prompt...)}
	for _, t := range call.Tools {
		req.Tools = append(req.Tools, t.GetName())
	}
	m.requests = append(m.requests, req)

	if len(m.replies) == 0 {
		return Reply{}, nil, fmt.Errorf("%w: request %d has no reply", ErrScriptExhausted, len(m.requests))
	}
	reply := m.replies[0]
	m.replies = m.replies[1:]

	ids := make([]string, len(reply.Calls))
	for i := range reply.Calls {
		m.callIDs++
		ids[i] = fmt.Sprintf("call_%d", m.callIDs)
	}
	return reply, ids, nil
}

func finishReason(r Reply) fantasy.FinishReason {
	if len(r.Calls) > 0 {
		return fantasy.FinishReasonToolCalls
	}
	return fantasy.FinishReasonStop
}

var usage = fantasy.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}

// languageModel adapts [Model] to fantasy.LanguageModel. It is unexported so
// fantasy does not appear in this package's API.
type languageModel struct{ m *Model }

var _ fantasy.LanguageModel = languageModel{}

func (l languageModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	reply, ids, err := l.m.answer(call)
	if err != nil {
		return nil, err
	}
	var content fantasy.ResponseContent
	if reply.Text != "" {
		content = append(content, fantasy.TextContent{Text: reply.Text})
	}
	for i, c := range reply.Calls {
		content = append(content, fantasy.ToolCallContent{ToolCallID: ids[i], ToolName: c.Name, Input: c.Input})
	}
	return &fantasy.Response{Content: content, FinishReason: finishReason(reply), Usage: usage}, nil
}

func (l languageModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	reply, ids, err := l.m.answer(call)
	if err != nil {
		return nil, err
	}
	var parts []fantasy.StreamPart
	if reply.Text != "" {
		parts = append(parts,
			fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: reply.Text},
			fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"},
		)
	}
	for i, c := range reply.Calls {
		parts = append(parts, fantasy.StreamPart{
			Type: fantasy.StreamPartTypeToolCall, ID: ids[i], ToolCallName: c.Name, ToolCallInput: c.Input,
		})
	}
	parts = append(parts, fantasy.StreamPart{
		Type: fantasy.StreamPartTypeFinish, FinishReason: finishReason(reply), Usage: usage,
	})
	return iter.Seq[fantasy.StreamPart](func(yield func(fantasy.StreamPart) bool) {
		for _, p := range parts {
			if !yield(p) {
				return
			}
		}
	}), nil
}

func (languageModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("fakemodel: structured output is not scripted")
}

func (languageModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("fakemodel: structured output is not scripted")
}

func (languageModel) Provider() string { return Provider }
func (languageModel) Model() string    { return ModelName }
