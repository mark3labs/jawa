package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type mcpRefreshPRArgs struct {
	ProjectID string `json:"project_id"`
	CardID    string `json:"card_id,omitempty"`
}
type mcpFixArgs struct {
	CardID  string `json:"card_id"`
	Message string `json:"message"`
	Agent   string `json:"agent,omitempty"`
}
type mcpMergeArgs struct {
	CardID         string `json:"card_id"`
	Snapshot       string `json:"snapshot"`
	Method         string `json:"method" jsonschema:"merge, squash or rebase"`
	IdempotencyKey string `json:"idempotency_key"`
}
type mcpMarkMergedArgs struct {
	CardID            string `json:"card_id"`
	ExpectedAttemptID string `json:"expected_attempt_id"`
	ExpectedPRURL     string `json:"expected_pr_url"`
}
type mcpCardIDArgs struct {
	CardID string `json:"card_id"`
}
type mcpForceReadyArgs struct {
	CardID      string `json:"card_id"`
	Expected    string `json:"expected"`
	PRNumber    string `json:"pr_number" jsonschema:"Positive PR number, or empty for the recorded PR"`
	Reason      string `json:"reason,omitempty"`
	Acknowledge bool   `json:"acknowledge"`
}
type mcpForceStopArgs struct {
	CardID      string `json:"card_id"`
	Acknowledge bool   `json:"acknowledge"`
}
type mcpDeleteArgs struct {
	CardID            string `json:"card_id"`
	Acknowledge       bool   `json:"acknowledge"`
	AcknowledgeOrphan bool   `json:"acknowledge_orphan,omitempty"`
}

func mcpMergeOperation(keyID, idem string) string {
	return tokenHash("mcp-merge\x00" + keyID + "\x00" + idem)[:32]
}

const mcpMergeLocked = "Merge is pending or its outcome is uncertain. Do not retry with a new idempotency_key. Call refresh_pr_checks for this project, then get_card."

func (m *mcpService) registerShipTools(srv *server.MCPServer) {
	addMCPLifecycleTool(m, srv, "refresh_pr_checks", "Refresh verified Done PR checks for a project or card.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpRefreshPRArgs) (*mcp.CallToolResult, error) {
		return m.actResult(ctx, "refresh_pr_checks", a.CardID, 35*time.Second, func(ctx context.Context) (map[string]any, error) {
			result, err := m.w.RefreshPRChecks(ctx, a.ProjectID, a.CardID)
			switch {
			case errors.Is(err, ErrPRRefreshBusy):
				err = errors.New("PR checks are already running. Wait, then try again")
			case errors.Is(err, ErrPRChecksUnavailable):
				err = errors.New("PR checks are unavailable. Try again when the provider checker is available")
			case errors.Is(err, sql.ErrNoRows):
				err = errors.New("Project or verified Done pull request not found")
			case err != nil:
				//nolint:staticcheck // Preserve the dashboard's user-facing error text.
				err = errors.New("Unable to refresh PR status. Unfinished checks remain queued; no clear status is assumed. Try again")
			}
			return map[string]any{"result": result}, err
		})
	})
	addMCPLifecycleTool(m, srv, "fix_card", "Queue repair of the existing verified PR; optional agent from list_agents.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpFixArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "fix_card", a.CardID, "", func(ctx context.Context) error {
			if a.Agent != "" {
				return m.w.FixWithAgent(ctx, a.CardID, a.Message, a.Agent)
			}
			return m.w.FixCard(a.CardID, a.Message)
		})
	})
	addMCPLifecycleTool(m, srv, "merge_pr", "Merge the listed snapshot. Reuse the same idempotency_key to retry.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpMergeArgs) (*mcp.CallToolResult, error) {
		return m.actResult(ctx, "merge_pr", a.CardID, 25*time.Second, func(ctx context.Context) (map[string]any, error) {
			if !idempotencyKeyPattern.MatchString(a.IdempotencyKey) {
				return nil, errors.New("idempotency_key must be 1–128 characters of A-Z a-z 0-9 . _ : -")
			}
			key := mcpKeyFromContext(ctx)
			err := m.w.MergePR(ctx, a.CardID, mcpMergeOperation(key.ID, a.IdempotencyKey), a.Snapshot, a.Method, "mcp:"+key.Name)
			if errors.Is(err, errMergeLocked) {
				//nolint:staticcheck // This is the client-facing merge recovery instruction.
				return nil, errors.New(mcpMergeLocked)
			}
			if err != nil {
				return nil, errMergeRejected
			}
			return map[string]any{"note": "Pull request merged."}, nil
		})
	})
	addMCPLifecycleTool(m, srv, "mark_pr_merged", "Record a manual merged marker for the listed attempt and PR.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpMarkMergedArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "mark_pr_merged", a.CardID, "", func(ctx context.Context) error {
			return m.w.MarkPRMerged(ctx, a.CardID, a.ExpectedAttemptID, a.ExpectedPRURL)
		})
	})
	for _, tool := range []string{"archive_card", "restore_card"} {
		addMCPLifecycleTool(m, srv, tool, "Change card visibility only; does not cancel active work.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpCardIDArgs) (*mcp.CallToolResult, error) {
			return m.act(ctx, tool, a.CardID, "", func(context.Context) error { return m.s.SetCardArchived(a.CardID, tool == "archive_card") })
		})
	}
	addMCPLifecycleTool(m, srv, "force_ready", "Operator override: bypass readiness checks, without merging. Requires acknowledgement.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpForceReadyArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "force_ready", a.CardID, "Card moved to Done. Readiness checks were bypassed; the PR was not merged.", func(ctx context.Context) error {
			return m.w.ForceReady(ctx, a.CardID, a.Expected, a.PRNumber, a.Reason, "mcp:"+mcpKeyFromContext(ctx).Name, a.Acknowledge)
		})
	})
	addMCPLifecycleTool(m, srv, "force_stop", "Fence the run; the agent may still be running. Requires acknowledgement.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpForceStopArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "force_stop", a.CardID, "Run force-stopped. Later reports from it are ignored; the agent was asked to stop but may still be running.", func(context.Context) error {
			if !a.Acknowledge {
				return errors.New("confirm that the agent may still be running before force stopping")
			}
			return m.w.ForceStop(a.CardID)
		})
	})
	addMCPLifecycleTool(m, srv, "delete_card", "Delete an inactive card. Requires acknowledgement; acknowledge_orphan permits unreconciled deletion.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpDeleteArgs) (*mcp.CallToolResult, error) {
		return m.actResult(ctx, "delete_card", a.CardID, 20*time.Second, func(context.Context) (map[string]any, error) {
			if !a.Acknowledge {
				return nil, errors.New("confirm deletion before continuing")
			}
			var err error
			if a.AcknowledgeOrphan {
				err = m.w.DeleteUnreconciledCard(a.CardID)
			} else {
				err = m.w.DeleteCard(a.CardID)
			}
			return map[string]any{"deleted": true, "card_id": a.CardID}, err
		})
	})
}
