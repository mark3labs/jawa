package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mark3labs/bonnie/runtime"
	"jawa/internal/testmodel"
)

// Read only the private smoke database and journal; no workflow state is faked.
func resumeSnapshot(ctx context.Context, dir, card string, healthy *testmodel.Model) (snapshot map[string]any, err error) {
	db, err := sql.Open("sqlite", filepath.Join(dir, "server", "orchestrator.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	result := map[string]any{}
	queries := map[string]string{
		"attempts": `SELECT * FROM workflow_attempts WHERE card_id=?`,
		"sessions": `SELECT * FROM planning_sessions WHERE card_id=?`,
		"turns":    `SELECT t.* FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id WHERE s.card_id=?`,
		"resumes":  `SELECT * FROM failed_resumes WHERE card_id=?`,
		"reports":  `SELECT r.* FROM typed_reports r WHERE r.task_id IN (SELECT task_id FROM workflow_attempts WHERE card_id=? UNION SELECT t.id FROM planning_turns t JOIN planning_sessions s ON s.id=t.session_id WHERE s.card_id=?)`,
	}
	run := ""
	for key, query := range queries {
		args := []any{card}
		if key == "reports" {
			args = append(args, card)
		}
		rows, e := db.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		columns, e := rows.Columns()
		if e != nil {
			return nil, errors.Join(e, rows.Close())
		}
		items := []map[string]any{}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if e = rows.Scan(pointers...); e != nil {
				return nil, errors.Join(e, rows.Close())
			}
			item := map[string]any{}
			for i, c := range columns {
				item[c] = values[i]
			}
			items = append(items, item)
			if id, ok := item["run_id"].(string); ok && id != "" {
				run = id
			}
		}
		e = errors.Join(rows.Err(), rows.Close())
		if e != nil {
			return nil, e
		}
		result[key] = items
	}
	if run == "" {
		result["records"] = []runtime.Record{}
		result["prompts"] = []string{}
		return result, nil
	}
	journal, e := runtime.OpenSQLiteJournal(filepath.Join(dir, "agent"))
	if e != nil {
		return nil, e
	}
	defer func() { err = errors.Join(err, journal.Close()) }()
	records, e := journal.Replay(ctx, run)
	if e != nil {
		return nil, e
	}
	result["records"] = records
	marker, e := os.ReadFile(filepath.Join(dir, "workspaces", run, "resume-marker.txt"))
	if e == nil {
		result["workspaceMarker"] = string(marker)
	}
	prompts := []string{}
	if healthy != nil {
		for _, r := range healthy.Requests() {
			prompts = append(prompts, r.Text())
		}
	}
	result["prompts"] = prompts
	return result, nil
}

// Add a context marker only while the real host is stopped, as in runtime tests.
func seedResumeContext(ctx context.Context, dir, card string) (err error) {
	snapshot, e := resumeSnapshot(ctx, dir, card, nil)
	if e != nil {
		return e
	}
	records := snapshot["records"].([]runtime.Record)
	if len(records) == 0 {
		return fmt.Errorf("empty journal")
	}
	j, e := runtime.OpenSQLiteJournal(filepath.Join(dir, "agent"))
	if e != nil {
		return e
	}
	defer func() { err = errors.Join(err, j.Close()) }()
	_, e = j.Append(ctx, runtime.Record{RunID: records[0].RunID, Kind: runtime.RecordContext, Text: "browser-saved-context-marker", Payload: []byte(`{"context":["browser-saved-context-marker"]}`)})
	return e
}

func writeResumeSnapshot(w http.ResponseWriter, r *http.Request, dir string, healthy *testmodel.Model) {
	data, e := resumeSnapshot(r.Context(), dir, r.URL.Query().Get("card"), healthy)
	if e != nil {
		http.Error(w, e.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if e = json.NewEncoder(w).Encode(data); e != nil {
		return
	}
}
