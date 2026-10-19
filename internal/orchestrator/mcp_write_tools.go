package orchestrator

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type mcpCreateProjectArgs struct {
	Name           string `json:"name" jsonschema:"Project name"`
	Provider       string `json:"provider,omitempty" jsonschema:"Provider: github or forgejo"`
	Repo           string `json:"repo,omitempty" jsonschema:"Repository URL (instead of repo_name)"`
	RepoName       string `json:"repo_name,omitempty" jsonschema:"Repository owner/name"`
	ForgejoURL     string `json:"forgejo_url,omitempty" jsonschema:"Forgejo HTTPS instance root"`
	BaseBranch     string `json:"base_branch,omitempty" jsonschema:"Base branch"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Required retry identity: 1–128 characters A-Z a-z 0-9 . _ : -"`
}
type mcpCreateCardArgs struct {
	ProjectID      string `json:"project_id" jsonschema:"Project ID"`
	Title          string `json:"title" jsonschema:"Card title"`
	Description    string `json:"description,omitempty" jsonschema:"Card description"`
	IssueURL       string `json:"issue_url,omitempty" jsonschema:"Existing issue URL"`
	AutoMerge      string `json:"auto_merge,omitempty" jsonschema:"Auto merge mode: inherit (default), on or off"`
	CreateIssue    bool   `json:"create_issue,omitempty" jsonschema:"Create a provider issue"`
	IssueRecovery  string `json:"issue_recovery,omitempty" jsonschema:"Recover an uncertain issue submission: link or unlinked"`
	RecoveryURL    string `json:"recovery_url,omitempty" jsonschema:"Issue URL for link recovery"`
	IdempotencyKey string `json:"idempotency_key" jsonschema:"Required retry identity: 1–128 characters A-Z a-z 0-9 . _ : -"`
}
type mcpSetAutoMergeArgs struct {
	CardID       string `json:"card_id" jsonschema:"Card ID"`
	Mode         string `json:"mode" jsonschema:"New mode: inherit, on or off"`
	ExpectedMode string `json:"expected_mode" jsonschema:"Exact current mode from actions"`
}
type mcpStartPlanningArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
	Agent  string `json:"agent,omitempty" jsonschema:"Optional ID from list_agents; empty uses shared queue"`
}
type mcpReplyPlanningArgs struct {
	CardID              string `json:"card_id" jsonschema:"Card ID"`
	Reply               string `json:"reply" jsonschema:"Nonblank reply, at most 65536 characters"`
	ExpectedInteraction string `json:"expected_interaction" jsonschema:"Exact interaction token from actions"`
}
type mcpApprovePlanArgs struct {
	CardID   string `json:"card_id" jsonschema:"Card ID"`
	Revision int    `json:"revision" jsonschema:"Exact reviewed revision from actions (at least 1)"`
	Agent    string `json:"agent,omitempty" jsonschema:"Optional ID from list_agents"`
}
type mcpRetryPlanningArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpCancelPlanningArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpStartBuildArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
	Agent  string `json:"agent,omitempty" jsonschema:"Optional ID from list_agents"`
}
type mcpRetryBuildArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpResumeFailedArgs struct {
	CardID         string `json:"card_id" jsonschema:"Card ID"`
	ExpectedResume string `json:"expected_resume" jsonschema:"Exact resume token from actions"`
}
type mcpGuideBlockedArgs struct {
	CardID              string `json:"card_id" jsonschema:"Card ID"`
	Message             string `json:"message" jsonschema:"Nonblank guidance, at most 64000 characters"`
	ExpectedInteraction string `json:"expected_interaction" jsonschema:"Exact interaction token from actions"`
}
type mcpCancelRunArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpResetCardArgs struct {
	CardID string `json:"card_id" jsonschema:"Card ID"`
}
type mcpMoveCardArgs struct {
	CardID   string `json:"card_id" jsonschema:"Card ID"`
	Position int    `json:"position" jsonschema:"Zero-based position within current lane"`
}

const mcpNATSGate = "NATS authentication setup is required. Finish Settings in the dashboard first."

func addMCPLifecycleTool[T any](m *mcpService, srv *server.MCPServer, name, description string, handler mcp.TypedToolHandlerFunc[T]) {
	destructive := name == "cancel_run" || name == "cancel_planning" || name == "reset_card" || name == "merge_pr" || name == "force_ready" || name == "force_stop" || name == "delete_card"
	opts := []mcp.ToolOption{mcp.WithDescription("JAWA orchestrator: " + description), mcp.WithInputSchema[T](), mcp.WithDestructiveHintAnnotation(destructive)}
	if name == "create_project" || name == "create_card" || name == "merge_pr" || name == "archive_card" || name == "restore_card" || name == "mark_pr_merged" {
		opts = append(opts, mcp.WithIdempotentHintAnnotation(true))
	}
	srv.AddTool(mcp.NewTool(name, opts...), mcp.NewTypedToolHandler(func(ctx context.Context, r mcp.CallToolRequest, args T) (*mcp.CallToolResult, error) {
		if ready, err := m.s.NATSConfigured(); err != nil || !ready {
			return mcp.NewToolResultError(mcpNATSGate), nil
		}
		timeout := 30 * time.Second
		if name == "refresh_pr_checks" {
			timeout = 35 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return handler(ctx, r, args)
	}))
}

func (m *mcpService) actionLog(ctx context.Context, tool, id string, err error) {
	message := "MCP action applied"
	if err != nil {
		message = "MCP action rejected"
	}
	m.log().Info(message, "tool", tool, "card_id", id, "api_key_id", mcpKeyFromContext(ctx).ID)
}
func mcpRejected(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError("Unable to apply request: " + strings.TrimRight(err.Error(), ". \t\n") + ". Call get_card for the current state and actions."), nil
}
func (m *mcpService) act(ctx context.Context, tool, cardID, note string, run func(context.Context) error) (*mcp.CallToolResult, error) {
	return m.actResult(ctx, tool, cardID, 20*time.Second, func(ctx context.Context) (map[string]any, error) {
		err := run(ctx)
		if note != "" {
			return map[string]any{"note": note}, err
		}
		return nil, err
	})
}

// actResult bounds and audits writes, then attaches the current card when applicable.
func (m *mcpService) actResult(ctx context.Context, tool, cardID string, timeout time.Duration, run func(context.Context) (map[string]any, error)) (*mcp.CallToolResult, error) {
	if ready, err := m.s.NATSConfigured(); err != nil || !ready {
		return mcp.NewToolResultError(mcpNATSGate), nil
	}
	if m.w == nil {
		return mcp.NewToolResultError("workflow is unavailable"), nil
	}
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	extra, err := run(actx)
	m.actionLog(ctx, tool, cardID, err)
	if err != nil {
		return mcpRejected(err)
	}
	result := map[string]any{"ok": true}
	maps.Copy(result, extra)
	if extra["deleted"] != true && cardID != "" {
		card, err := m.card(actx, cardID)
		if err != nil {
			return mcp.NewToolResultError("Applied, but the card could not be reloaded; call get_card."), nil
		}
		result["card"] = card
	}
	return mcpJSON(result)
}
func (m *mcpService) currentCard(id string) (Card, error) {
	cards, err := m.s.Cards("")
	if err != nil {
		return Card{}, err
	}
	c, ok := selectedCard(cards, id)
	if !ok {
		return Card{}, errMCPCardNotFound
	}
	return c, nil
}

func (m *mcpService) registerLifecycleTools(srv *server.MCPServer) {
	addMCPLifecycleTool(m, srv, "create_project", "Create a project; reuse idempotency_key to retry safely.", m.createProject)
	addMCPLifecycleTool(m, srv, "create_card", `Create a card, optionally creating an issue. Reuse idempotency_key for retries. If issue creation is uncertain, check the repository, then call again with the same idempotency_key, project_id, title, description and auto_merge, plus issue_recovery: link and recovery_url, or issue_recovery: unlinked (omit create_issue). The ledger rejects draft changes. To retry definitely rejected creation use a new idempotency_key.`, m.createCard)
	addMCPLifecycleTool(m, srv, "set_auto_merge", "Set card auto merge policy.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpSetAutoMergeArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "set_auto_merge", a.CardID, "", func(context.Context) error { return m.w.SetCardAutoMerge(a.CardID, a.ExpectedMode, a.Mode) })
	})
	addMCPLifecycleTool(m, srv, "start_planning", "Queue planning.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpStartPlanningArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "start_planning", a.CardID, "", func(ctx context.Context) error {
			if a.Agent != "" {
				return m.w.PlanWithAgent(ctx, a.CardID, a.Agent)
			}
			return m.w.MoveCard(a.CardID, "Planning", 0)
		})
	})
	addMCPLifecycleTool(m, srv, "reply_planning", "Answer planning questions or guide a revision.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpReplyPlanningArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "reply_planning", a.CardID, "", func(context.Context) error {
			if strings.TrimSpace(a.Reply) == "" || utf8.RuneCountInString(a.Reply) > 65536 {
				return errors.New("reply must be nonblank and at most 65536 characters")
			}
			return m.w.ReplyPlanning(a.CardID, a.Reply, a.ExpectedInteraction)
		})
	})
	addMCPLifecycleTool(m, srv, "approve_plan", "Approve the exact reviewed plan revision and queue a build.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpApprovePlanArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "approve_plan", a.CardID, "", func(ctx context.Context) error {
			if a.Revision < 1 {
				return errors.New("select a valid plan revision to approve")
			}
			if a.Agent != "" {
				return m.w.ApprovePlanningWithAgent(ctx, a.CardID, a.Revision, a.Agent)
			}
			return m.w.ApprovePlanning(a.CardID, a.Revision)
		})
	})
	addMCPLifecycleTool(m, srv, "retry_planning", "Queue a fresh planning attempt.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpRetryPlanningArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "retry_planning", a.CardID, "", func(context.Context) error { return m.w.RetryPlanning(a.CardID) })
	})
	addMCPLifecycleTool(m, srv, "cancel_planning", "Cancel planning and return to Todo.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpCancelPlanningArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "cancel_planning", a.CardID, "", func(context.Context) error { return m.w.CancelPlanning(a.CardID) })
	})
	addMCPLifecycleTool(m, srv, "start_build", "Queue a build from Todo. Planning requires approve_plan.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpStartBuildArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "start_build", a.CardID, "", func(ctx context.Context) error {
			c, err := m.currentCard(a.CardID)
			if err != nil {
				return err
			}
			if c.Status == "Planning" {
				return errors.New("approve a plan revision with approve_plan before building")
			}
			if a.Agent != "" {
				return m.w.BuildWithAgent(ctx, a.CardID, a.Agent)
			}
			return m.w.MoveCard(a.CardID, "Building", 0)
		})
	})
	addMCPLifecycleTool(m, srv, "retry_build", "Queue a fresh build attempt or start legacy Building work.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpRetryBuildArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "retry_build", a.CardID, "", func(context.Context) error { return m.w.Retry(a.CardID) })
	})
	addMCPLifecycleTool(m, srv, "resume_failed", "Resume the exact failed planning or build run.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpResumeFailedArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "resume_failed", a.CardID, "", func(context.Context) error {
			if a.ExpectedResume == "" {
				return errors.New("refresh before resuming")
			}
			c, err := m.currentCard(a.CardID)
			if err != nil {
				return err
			}
			return m.w.resumeFailed(a.CardID, c.Status == "Planning", a.ExpectedResume)
		})
	})
	addMCPLifecycleTool(m, srv, "guide_blocked", "Answer the current build blocker.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpGuideBlockedArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "guide_blocked", a.CardID, "", func(context.Context) error {
			if strings.TrimSpace(a.Message) == "" || utf8.RuneCountInString(a.Message) > 64000 {
				return errors.New("message must be nonblank and at most 64000 characters")
			}
			return m.w.GuideBlocked(a.CardID, a.Message, a.ExpectedInteraction)
		})
	})
	addMCPLifecycleTool(m, srv, "cancel_run", "Request cancellation, not confirmation.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpCancelRunArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "cancel_run", a.CardID, "Cancellation was requested; it is not confirmed until the agent reports cancellation. External actions already taken cannot be undone.", func(ctx context.Context) error { return m.w.CancelCard(ctx, a.CardID) })
	})
	addMCPLifecycleTool(m, srv, "reset_card", "Reset an inactive card to Todo, preserving history.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpResetCardArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "reset_card", a.CardID, "", func(context.Context) error { return m.w.ResetCard(a.CardID) })
	})
	addMCPLifecycleTool(m, srv, "move_card", "Reorder within the current lane; does not change lane.", func(ctx context.Context, _ mcp.CallToolRequest, a mcpMoveCardArgs) (*mcp.CallToolResult, error) {
		return m.act(ctx, "move_card", a.CardID, "", func(context.Context) error {
			if a.Position < 0 {
				return errors.New("invalid card position")
			}
			c, err := m.currentCard(a.CardID)
			if err != nil {
				return err
			}
			return m.w.MoveCard(a.CardID, c.Status, a.Position)
		})
	})
}

func (m *mcpService) createProject(ctx context.Context, _ mcp.CallToolRequest, a mcpCreateProjectArgs) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	idem := a.IdempotencyKey
	a.IdempotencyKey = ""
	id, replayed, err := m.s.replayOrRun(ctx, mcpKeyFromContext(ctx).ID, idem, "create_project", mcpArgsHash("create_project", a), func(tx *sql.Tx) (string, error) {
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		repo := a.Repo
		if a.RepoName != "" {
			if repo != "" {
				return "", errors.New("choose repo or repo_name, not both")
			}
			var err error
			repo, err = projectRepositoryURL(provider, a.RepoName, a.ForgejoURL)
			if err != nil {
				return "", err
			}
		}
		p, err := insertProjectDetails(tx, a.Name, provider, repo, a.BaseBranch)
		return p.ID, err
	})
	m.actionLog(ctx, "create_project", "", err)
	if err != nil {
		return mcpRejected(err)
	}
	projects, err := m.s.Projects()
	if err != nil {
		return m.toolError("create_project", err)
	}
	for _, p := range projects {
		if p.ID == id {
			return mcpJSON(map[string]any{"ok": true, "replayed": replayed, "project": mcpProject(p)})
		}
	}
	return mcp.NewToolResultError("the project created by this idempotency_key no longer exists"), nil
}

func (m *mcpService) createCard(ctx context.Context, _ mcp.CallToolRequest, a mcpCreateCardArgs) (*mcp.CallToolResult, error) {
	if !idempotencyKeyPattern.MatchString(a.IdempotencyKey) {
		return mcpRejected(errors.New("idempotency_key must be 1–128 characters of A-Z a-z 0-9 . _ : -"))
	}
	mode, err := submittedAutoMergeMode(a.AutoMerge)
	if err != nil {
		return mcpRejected(err)
	}
	var id string
	var replayed bool
	if a.CreateIssue || a.IssueRecovery != "" {
		if a.CreateIssue && a.IssueRecovery != "" {
			return mcp.NewToolResultError("Choose either create_issue or issue_recovery, not both."), nil
		}
		if a.IssueRecovery != "" && a.IssueRecovery != "link" && a.IssueRecovery != "unlinked" {
			return mcp.NewToolResultError("issue_recovery must be link or unlinked."), nil
		}
		token := tokenHash("mcp-issue\x00" + mcpKeyFromContext(ctx).ID + "\x00" + a.IdempotencyKey)
		// Reserve the key across both creation paths before provider I/O.
		// Recovery changes intent but must retain the original draft.
		draft := a
		draft.IdempotencyKey, draft.IssueRecovery, draft.RecoveryURL = "", "", ""
		draft.CreateIssue = true
		_, _, err = m.s.replayOrRun(ctx, mcpKeyFromContext(ctx).ID, a.IdempotencyKey, "create_card", mcpArgsHash("create_card", draft), func(*sql.Tx) (string, error) { return "", nil })
		if err != nil {
			return mcpRejected(err)
		}
		replayed = m.s.hasIssueSubmission(token)
		form := url.Values{"project_id": {a.ProjectID}, "title": {a.Title}, "description": {a.Description}, "auto_merge_mode": {mode}, "issue_submission": {token}, "issue_url": {a.IssueURL}}
		if a.CreateIssue {
			form.Set("create_issue", "yes")
		} else {
			form.Set("issue_recovery", a.IssueRecovery)
			form.Set("recovery_url", a.RecoveryURL)
		}
		ictx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		err = submitIssueCardForm(ictx, m.s, m.issueClient, form)
		if err != nil {
			m.actionLog(ctx, "create_card", "", err)
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err = m.s.db.QueryRowContext(ctx, `SELECT card_id FROM issue_submissions WHERE token=?`, token).Scan(&id); err != nil || id == "" {
			return mcp.NewToolResultError("Issue submission did not produce a card; call create_card again with the same idempotency_key."), nil
		}
		if _, err = m.s.db.ExecContext(ctx, `UPDATE mcp_requests SET result=? WHERE key_id=? AND idempotency_key=?`, id, mcpKeyFromContext(ctx).ID, a.IdempotencyKey); err != nil {
			return m.toolError("create_card", err)
		}
	} else {
		actx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		idem := a.IdempotencyKey
		a.IdempotencyKey = ""
		id, replayed, err = m.s.replayOrRun(actx, mcpKeyFromContext(ctx).ID, idem, "create_card", mcpArgsHash("create_card", a), func(tx *sql.Tx) (string, error) {
			c, err := insertCardOptions(tx, a.ProjectID, a.Title, a.Description, a.IssueURL, mode)
			return c.ID, err
		})
		if err != nil {
			m.actionLog(ctx, "create_card", "", err)
			return mcpRejected(err)
		}
	}
	m.actionLog(ctx, "create_card", id, nil)
	card, err := m.card(ctx, id)
	if errors.Is(err, errMCPCardNotFound) {
		return mcp.NewToolResultError("the card created by this idempotency_key no longer exists"), nil
	}
	if err != nil {
		return m.toolError("create_card", err)
	}
	return mcpJSON(map[string]any{"ok": true, "replayed": replayed, "card": card})
}
