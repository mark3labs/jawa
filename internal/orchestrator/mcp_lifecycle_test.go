package orchestrator

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// All operator decisions go through the authenticated HTTP endpoint. Only agent
// completion and provider responses are simulated in-process.
func TestMCPLifecycleEndToEnd(t *testing.T) {
	w := actionWorkflow(t)
	mcpSetup(t, w)
	_, secret, err := w.s.CreateAPIKey(t.Context(), "e2e")
	if err != nil {
		t.Fatal(err)
	}
	w.SetProviderChecks(nil, func(context.Context, Attempt) (PRCheck, error) {
		return PRCheck{State: "clear", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40)}, nil
	})
	var merges atomic.Int32
	w.SetProviderMerger(func(context.Context, Attempt, string, string) string { merges.Add(1); return "merged" })
	srv := httptest.NewServer((&mcpService{s: w.s, w: w}).handler())
	defer srv.Close()
	c := mcpTestClient(t, srv.URL, secret)
	call := func(tool string, args map[string]any) []byte {
		t.Helper()
		b := mcpCall(t, c, tool, args)
		if strings.Contains(string(b), secret) {
			t.Fatal("API secret in response")
		}
		return b
	}
	get := func(id string) mcpCard { return mcpDecode[mcpCard](t, call("get_card", map[string]any{"card_id": id})) }
	action := func(card mcpCard, tool string) map[string]any {
		t.Helper()
		for _, a := range card.Actions {
			if a.Tool == tool {
				return a.Args
			}
		}
		t.Fatalf("missing %s: %+v", tool, card)
		return nil
	}
	wait := func(card mcpCard) mcpCard {
		t.Helper()
		result := mcpDecode[struct {
			Changed bool    `json:"changed"`
			Card    mcpCard `json:"card"`
		}](t, call("wait_for_card", map[string]any{"card_id": card.ID, "version": card.Version, "timeout_seconds": 5}))
		if !result.Changed {
			t.Fatal("expected change")
		}
		return result.Card
	}
	pa := map[string]any{"name": "e2e", "provider": "github", "repo": "https://github.com/example/repo", "base_branch": "main", "idempotency_key": "p1"}
	p := mcpDecode[struct {
		Project mcpProject `json:"project"`
	}](t, call("create_project", pa)).Project
	call("create_project", pa)
	ca := map[string]any{"project_id": p.ID, "title": "Add feature", "idempotency_key": "c1"}
	card := mcpResultCard(t, call("create_card", ca))
	call("create_card", ca)
	if card.Lane != "Todo" {
		t.Fatal(card)
	}
	card = mcpResultCard(t, call("start_planning", action(card, "start_planning")))
	if err := planningDelivery(t, w, planningOutcome(t, w, card.ID, "Final plan")); err != nil {
		t.Fatal(err)
	}
	card = wait(card)
	args := action(card, "approve_plan")
	if args["revision"] != float64(1) {
		t.Fatal(args)
	}
	card = mcpResultCard(t, call("approve_plan", args))
	if card.Lane != "Building" {
		t.Fatal(card)
	}
	finish := func() {
		t.Helper()
		mustExec(t, w, `UPDATE workflow_attempts SET state='blocked',run_state='completed',outcome_json='{}',pr_url='https://github.com/example/repo/pull/18' WHERE card_id=? AND number=(SELECT max(number) FROM workflow_attempts WHERE card_id=?)`, card.ID, card.ID)
		a, err := w.CardResult(card.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.markReady(a); err != nil {
			t.Fatal(err)
		}
	}
	finish()
	card = wait(card)
	if card.Lane != "Done" || card.Status != "Ready for review" {
		t.Fatal(card)
	}
	action(card, "refresh_pr_checks")
	action(card, "mark_pr_merged")
	args = action(card, "fix_card")
	args["message"] = "tighten tests"
	card = mcpResultCard(t, call("fix_card", args))
	if card.Lane != "Fix" {
		t.Fatal(card)
	}
	finish()
	card = wait(card)
	refresh := mcpDecode[struct {
		Result PRRefreshResult `json:"result"`
	}](t, call("refresh_pr_checks", action(card, "refresh_pr_checks")))
	if refresh.Result.Queued > 0 {
		if err := w.PollPRConflicts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	card = get(card.ID)
	args = action(card, "merge_pr")
	args["method"], args["idempotency_key"] = "squash", "m1"
	call("merge_pr", args)
	card = get(card.ID)
	if card.Status != "Merged" {
		t.Fatal(card)
	}
	call("archive_card", action(card, "archive_card"))
	for _, archived := range []bool{false, true} {
		list := mcpDecode[struct {
			Cards []mcpCardSummary `json:"cards"`
		}](t, call("list_cards", map[string]any{"project_id": p.ID, "include_archived": archived}))
		if len(list.Cards) != map[bool]int{false: 0, true: 1}[archived] {
			t.Fatal(list)
		}
	}
	cards, err := w.s.Cards("")
	if err != nil || len(cards) != 1 {
		t.Fatal(cards, err)
	}
	projects, err := w.s.Projects()
	if err != nil || len(projects) != 1 || merges.Load() != 1 {
		t.Fatal(projects, err, merges.Load())
	}
}
