package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type mcpNoArgs struct{}
type mcpListCardsArgs struct {
	ProjectID       string `json:"project_id,omitempty" jsonschema:"Filter by project ID"`
	Lane            string `json:"lane,omitempty" jsonschema:"Filter by lane,enum=Todo,enum=Planning,enum=Building,enum=Fix,enum=Done"`
	IncludeArchived bool   `json:"include_archived,omitempty" jsonschema:"Include archived cards (default false)"`
}
type mcpGetCardArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpWaitArgs struct {
	CardID         string `json:"card_id" jsonschema:"Card ID"`
	Version        string `json:"version" jsonschema:"Last observed card version"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Timeout in seconds (1–25, default 20)"`
}

// All tool failures are user-facing tool results, never JSON-RPC errors.
func (m *mcpService) toolError(tool string, err error) (*mcp.CallToolResult, error) {
	if errors.Is(err, errMCPCardNotFound) {
		return mcp.NewToolResultError(err.Error()), nil
	}
	m.log().Error("MCP tool unavailable", "tool", tool)
	return mcp.NewToolResultError("Jawa is temporarily unavailable; try again."), nil
}
func addMCPReadTool[T any](srv *server.MCPServer, name, description string, timeout time.Duration, handler mcp.TypedToolHandlerFunc[T]) {
	srv.AddTool(mcp.NewTool(name, mcp.WithDescription("JAWA orchestrator: " + description), mcp.WithInputSchema[T](), mcp.WithReadOnlyHintAnnotation(true)), mcp.NewTypedToolHandler(func(ctx context.Context, r mcp.CallToolRequest, args T) (*mcp.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, r, args)
	}))
}
func (m *mcpService) registerReadTools(srv *server.MCPServer) {
	addMCPReadTool(srv, "list_projects", "List configured projects.", 10*time.Second, func(ctx context.Context, _ mcp.CallToolRequest, _ mcpNoArgs) (*mcp.CallToolResult, error) {
		d, err := m.snapshot(ctx)
		if err != nil {
			return m.toolError("list_projects", err)
		}
		projects := make([]mcpProject, 0, len(d.projects))
		for _, p := range d.projects {
			projects = append(projects, mcpProject(p))
		}
		return mcpJSON(map[string]any{"projects": projects})
	})
	addMCPReadTool(srv, "list_agents", "List discoverable agents without transport addresses.", 10*time.Second, func(ctx context.Context, _ mcp.CallToolRequest, _ mcpNoArgs) (*mcp.CallToolResult, error) {
		agents, err := m.w.choices(ctx)
		if agents == nil {
			agents = []AgentChoice{}
		}
		return mcpJSON(map[string]any{"available": err == nil, "agents": agents})
	})
	addMCPReadTool(srv, "list_cards", "List cards in board order; archived cards are hidden by default.", 10*time.Second, func(ctx context.Context, _ mcp.CallToolRequest, args mcpListCardsArgs) (*mcp.CallToolResult, error) {
		switch args.Lane {
		case "", "Todo", "Planning", "Building", "Fix", "Done":
		default:
			return mcp.NewToolResultError("invalid lane"), nil
		}
		d, err := m.snapshot(ctx)
		if err != nil {
			return m.toolError("list_cards", err)
		}
		cards := []mcpCardSummary{}
		for _, c := range d.cards {
			if (args.ProjectID != "" && c.ProjectID != args.ProjectID) || (args.Lane != "" && c.Status != args.Lane) || (c.Archived && !args.IncludeArchived) {
				continue
			}
			cards = append(cards, projectCard(d, c).mcpCardSummary)
		}
		return mcpJSON(map[string]any{"cards": cards})
	})
	addMCPReadTool(srv, "get_card", "Get information visible on a card and its plan page.", 10*time.Second, func(ctx context.Context, _ mcp.CallToolRequest, args mcpGetCardArgs) (*mcp.CallToolResult, error) {
		card, err := m.card(ctx, args.CardID)
		if err != nil {
			return m.toolError("get_card", err)
		}
		return mcpJSON(card)
	})
	addMCPReadTool(srv, "wait_for_card", "Return as soon as the card changes or at the timeout. Clients should loop with the returned version to follow progress.", 30*time.Second, func(ctx context.Context, _ mcp.CallToolRequest, args mcpWaitArgs) (*mcp.CallToolResult, error) {
		timeout := args.TimeoutSeconds
		if timeout == 0 {
			timeout = 20
		}
		if timeout < 1 || timeout > 25 {
			return mcp.NewToolResultError("timeout_seconds must be between 1 and 25"), nil
		}
		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		ticker := time.NewTicker(mcpWaitPollInterval)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return mcp.NewToolResultError("wait cancelled"), nil
			}
			card, err := m.card(ctx, args.CardID)
			if err != nil {
				return m.toolError("wait_for_card", err)
			}
			if card.Version != args.Version || !time.Now().Before(deadline) {
				return mcpJSON(map[string]any{"changed": card.Version != args.Version, "card": card})
			}
			select {
			case <-ctx.Done():
				return mcp.NewToolResultError("wait cancelled"), nil
			case <-ticker.C:
			}
		}
	})
}
