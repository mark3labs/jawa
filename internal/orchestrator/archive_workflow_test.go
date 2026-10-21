package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func insertPendingArchiveMerge(t *testing.T, s *Store, id string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO pr_merges VALUES(?,?,'attempt','url','snapshot','github','repo','main','head','squash','operator','pending',0,0)`, "merge-"+id, id)
	if err != nil {
		t.Fatal(err)
	}
}

func TestArchiveWorkflowHandlers(t *testing.T) {
	for _, transport := range []string{"http", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			w := actionWorkflow(t)
			mcpSetup(t, w)
			c := workflowCard(t, w.s)
			a := &app{s: w.s, workflow: w}
			service := &mcpService{s: w.s, w: w}
			srv := server.NewMCPServer("test", "1")
			service.registerShipTools(srv)
			archive := func(ctx context.Context, archived bool) error {
				if transport == "http" {
					path := "/cards/archive"
					if !archived {
						path = "/cards/restore"
					}
					r := httptest.NewRequest("POST", path, strings.NewReader(url.Values{"card_id": {c.ID}}.Encode())).WithContext(ctx)
					r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					return a.mutateCardsArchive(httptest.NewRecorder(), r, &mutationContext{})
				}
				tool := "archive_card"
				if !archived {
					tool = "restore_card"
				}
				var req mcp.CallToolRequest
				req.Params.Name, req.Params.Arguments = tool, map[string]any{"card_id": c.ID}
				result, err := srv.GetTool(tool).Handler(ctx, req)
				if err != nil {
					return err
				}
				if result.IsError {
					b, _ := json.Marshal(result.Content)
					return errors.New(string(b))
				}
				return nil
			}
			assertArchived := func(want bool) {
				t.Helper()
				got, err := w.s.cardByID(t.Context(), c.ID)
				if err != nil || got.Archived != want || got.Status != c.Status {
					t.Fatalf("card=%+v err=%v", got, err)
				}
			}
			for _, archived := range []bool{true, false} {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := archive(ctx, archived); err == nil {
					t.Fatal("cancelled archive succeeded")
				}
				assertArchived(!archived)
				insertPendingArchiveMerge(t, w.s, c.ID)
				if err := archive(t.Context(), archived); err == nil {
					t.Fatal("unresolved merge allowed archive")
				}
				assertArchived(!archived)
				if _, err := w.s.db.Exec(`DELETE FROM pr_merges`); err != nil {
					t.Fatal(err)
				}
				if err := archive(t.Context(), archived); err != nil {
					t.Fatal(err)
				}
				assertArchived(archived)
			}
		})
	}
}

func TestPlanningBuildLookupHandlers(t *testing.T) {
	w := actionWorkflow(t)
	c := workflowCard(t, w.s)
	if err := w.MoveCard(c.ID, "Planning", 0); err != nil {
		t.Fatal(err)
	}
	a := &app{s: w.s, workflow: w}
	for _, id := range []string{c.ID, "missing"} {
		r := httptest.NewRequest("POST", "/cards/move", nil)
		r.Form = url.Values{"card_id": {id}, "status": {"Building"}}
		err := a.mutateCardsMove(httptest.NewRecorder(), r, &mutationContext{})
		want := "review and approve a plan revision before building"
		if id == "missing" {
			want = "card not found"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("id=%s err=%v", id, err)
		}
	}
	client := mcpWorkflowClient(t, w, nil)
	mcpReject(t, client, "start_build", map[string]any{"card_id": c.ID}, "approve a plan revision with approve_plan before building")
	mcpReject(t, client, "start_build", map[string]any{"card_id": "missing"}, "card not found")
}

func TestArchiveStartupSweep(t *testing.T) {
	path := t.TempDir() + "/board.db"
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	c := workflowCard(t, s)
	insertPendingArchiveMerge(t, s, c.ID)
	if _, _, err := s.reserveIssueSubmission(strings.Repeat("a", 64), Project{ID: c.ProjectID}, "title", "body"); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"reserved", "inflight"} {
		if _, err := s.db.Exec(`INSERT INTO auto_merge_operations VALUES(?,?,?,'github','repo','main','url','head','config',?,'',0,0,0,0)`, state, state, state, state); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for table, want := range map[string]int{"pr_merges": 1, "issue_submissions": 1, "auto_merge_operations": 2} {
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table + ` WHERE state='uncertain'`).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
}
