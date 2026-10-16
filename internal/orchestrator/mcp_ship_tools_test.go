package orchestrator

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

var shipTools = []string{"refresh_pr_checks", "fix_card", "merge_pr", "mark_pr_merged", "archive_card", "restore_card", "force_ready", "force_stop", "delete_card"}

func TestMCPShipAnnotationsAndGate(t *testing.T) {
	_, url, secret := mcpTestServer(t)
	c := mcpTestClient(t, url, secret)
	tools, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range shipTools {
		mcpReject(t, c, name, map[string]any{"card_id": "absent", "project_id": "absent", "message": "fix", "snapshot": "x", "method": "merge", "idempotency_key": "k", "expected_attempt_id": "x", "expected_pr_url": "x", "expected": "x", "pr_number": "1", "acknowledge": true}, "NATS authentication setup")
		found := false
		for _, tool := range tools.Tools {
			if tool.Name != name {
				continue
			}
			found = true
			destructive := name == "merge_pr" || name == "force_ready" || name == "force_stop" || name == "delete_card"
			if tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != destructive {
				t.Fatal(name, "destructive")
			}
			if name == "merge_pr" || name == "archive_card" || name == "restore_card" || name == "mark_pr_merged" {
				if tool.Annotations.IdempotentHint == nil || !*tool.Annotations.IdempotentHint {
					t.Fatal(name, "idempotent")
				}
			}
		}
		if !found {
			t.Fatal(name)
		}
	}
}

func TestMCPMergeIsIdempotentAndAttributed(t *testing.T) {
	for _, outcome := range []string{"merged", "uncertain"} {
		t.Run(outcome, func(t *testing.T) {
			w, card, _, _ := mergeFixture(t)
			var calls atomic.Int32
			w.SetProviderMerger(func(context.Context, Attempt, string, string) string { calls.Add(1); return outcome })
			c := mcpWorkflowClient(t, w, nil)
			args := mcpActionArgs(t, c, card.ID, "merge_pr")
			args["idempotency_key"] = "m1"
			for range 2 {
				if outcome == "merged" {
					mcpCall(t, c, "merge_pr", args)
				} else {
					mcpReject(t, c, "merge_pr", args, "Do not retry with a new idempotency_key")
				}
			}
			if calls.Load() != 1 {
				t.Fatal(calls.Load())
			}
			var operator string
			if err := w.s.db.QueryRow(`SELECT operator FROM pr_merges`).Scan(&operator); err != nil || operator != "mcp:lifecycle" {
				t.Fatal(operator, err)
			}
			if outcome == "merged" {
				got := mcpDecode[mcpCard](t, mcpCall(t, c, "get_card", map[string]any{"card_id": card.ID}))
				if got.Status != "Merged" {
					t.Fatal(got)
				}
				for _, a := range got.Actions {
					if a.Tool == "merge_pr" {
						t.Fatal("merged card offers merge")
					}
				}
				args["snapshot"], args["idempotency_key"] = "stale", "m2"
				mcpReject(t, c, "merge_pr", args, errMergeRejected.Error())
			}
			if calls.Load() != 1 {
				t.Fatal(calls.Load())
			}
		})
	}
}

func TestMCPMarkMergedAndFix(t *testing.T) {
	w := actionWorkflow(t)
	card := workflowCard(t, w.s)
	repairDone(t, w, card)
	c := mcpWorkflowClient(t, w, nil)
	args := mcpActionArgs(t, c, card.ID, "mark_pr_merged")
	original := args["expected_pr_url"]
	args["expected_pr_url"] = "https://github.com/example/repo/pull/999"
	mcpReject(t, c, "mark_pr_merged", args, "Unable to apply request")
	args["expected_pr_url"] = original
	mcpCall(t, c, "mark_pr_merged", args)
	other := workflowCard(t, w.s)
	repairDone(t, w, other)
	args = mcpActionArgs(t, c, other.ID, "fix_card")
	args["message"] = ""
	mcpReject(t, c, "fix_card", args, "Unable to apply request")
	args["message"] = "tighten tests"
	got := mcpResultCard(t, mcpCall(t, c, "fix_card", args))
	a, err := w.CardResult(other.ID)
	if err != nil || got.Lane != "Fix" || a.RepairMessage != "tighten tests" {
		t.Fatal(got, a, err)
	}
	cw, cc, ca, _ := mergeFixture(t)
	mustExec(t, cw, `UPDATE workflow_pr_checks SET state='conflict' WHERE attempt_id=?`, ca.ID)
	client := mcpWorkflowClient(t, cw, nil)
	if mcpActionArgs(t, client, cc.ID, "fix_card")["message"] != conflictFixMessage {
		t.Fatal("missing conflict message")
	}
}

func TestMCPRefreshPRChecks(t *testing.T) {
	w, card, _, _ := mergeFixture(t)
	c := mcpWorkflowClient(t, w, nil)
	args := map[string]any{"project_id": card.ProjectID, "card_id": card.ID}
	result := mcpDecode[struct {
		Result PRRefreshResult `json:"result"`
	}](t, mcpCall(t, c, "refresh_pr_checks", args))
	if result.Result.Targeted < 1 {
		t.Fatal(result)
	}
	w.SetProviderChecks(nil, nil)
	mcpReject(t, c, "refresh_pr_checks", args, "PR checks are unavailable")
	args["project_id"] = "missing"
	mcpReject(t, c, "refresh_pr_checks", args, "Project or verified Done pull request not found")
}

func TestMCPArchiveRestore(t *testing.T) {
	w := actionWorkflow(t)
	card := workflowCard(t, w.s)
	actionAttempt(t, w, card, "running", "running")
	c := mcpWorkflowClient(t, w, nil)
	got := mcpResultCard(t, mcpCall(t, c, "archive_card", map[string]any{"card_id": card.ID}))
	if !got.Archived {
		t.Fatal(got)
	}
	mcpActionArgs(t, c, card.ID, "restore_card")
	for _, archived := range []bool{false, true} {
		list := mcpDecode[struct {
			Cards []mcpCardSummary `json:"cards"`
		}](t, mcpCall(t, c, "list_cards", map[string]any{"include_archived": archived}))
		if len(list.Cards) != map[bool]int{false: 0, true: 1}[archived] {
			t.Fatal(list)
		}
	}
	got = mcpResultCard(t, mcpCall(t, c, "restore_card", map[string]any{"card_id": card.ID}))
	if got.Archived {
		t.Fatal(got)
	}
}

func TestMCPForceReadyRequiresAcknowledgeAndAttributes(t *testing.T) {
	w, card, _, _ := readyFixture(t)
	c := mcpWorkflowClient(t, w, nil)
	args := mcpActionArgs(t, c, card.ID, "force_ready")
	args["pr_number"], args["acknowledge"] = "55", false
	mcpReject(t, c, "force_ready", args, "confirm that CI, review and readiness checks are bypassed")
	assertCardStatus(t, w, card.ID, "Building")
	args["acknowledge"] = true
	got := mcpResultCard(t, mcpCall(t, c, "force_ready", args))
	if got.Lane != "Done" {
		t.Fatal(got)
	}
	var operator string
	if err := w.s.db.QueryRow(`SELECT operator FROM workflow_ready_overrides`).Scan(&operator); err != nil || operator != "mcp:lifecycle" {
		t.Fatal(operator, err)
	}
	args["expected"] = "stale"
	mcpReject(t, c, "force_ready", args, "Unable to apply request")
}

func TestMCPForceStopAndDelete(t *testing.T) {
	w := actionWorkflow(t)
	card := workflowCard(t, w.s)
	actionAttempt(t, w, card, "running", "running")
	c := mcpWorkflowClient(t, w, nil)
	full := mcpDecode[mcpCard](t, mcpCall(t, c, "get_card", map[string]any{"card_id": card.ID}))
	found := false
	for _, u := range full.Unavailable {
		if u.Tool == "delete_card" && strings.Contains(strings.ToLower(u.Reason), "active attempt") {
			found = true
		}
	}
	if !found {
		t.Fatal(full)
	}
	args := mcpActionArgs(t, c, card.ID, "force_stop")
	args["acknowledge"] = false
	mcpReject(t, c, "force_stop", args, "confirm that the agent may still be running before force stopping")
	args["acknowledge"] = true
	mcpCall(t, c, "force_stop", args)
	args = mcpActionArgs(t, c, card.ID, "delete_card")
	args["acknowledge"] = false
	mcpReject(t, c, "delete_card", args, "confirm deletion")
	args["acknowledge"] = true
	result := mcpDecode[struct {
		Deleted bool `json:"deleted"`
	}](t, mcpCall(t, c, "delete_card", args))
	if !result.Deleted {
		t.Fatal(result)
	}
	mcpReject(t, c, "get_card", map[string]any{"card_id": card.ID}, "card not found")
	other := workflowCard(t, w.s)
	a := actionAttempt(t, w, other, "blocked", "completed")
	mustExec(t, w, `UPDATE workflow_attempts SET error=? WHERE id=?`, executionConflictNotice, a.ID)
	args = mcpActionArgs(t, c, other.ID, "delete_card")
	if args["acknowledge_orphan"] != true {
		t.Fatal(args)
	}
	args["acknowledge"] = true
	mcpCall(t, c, "delete_card", args)
}
