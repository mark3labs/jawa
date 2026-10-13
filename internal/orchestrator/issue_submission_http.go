package orchestrator

import (
	"html"
	"net/http"
	"strings"
)

// Failure patches only recovery controls, never draft fields or the dialog.
func (a *app) issueCardError(w http.ResponseWriter, r *http.Request, err error) {
	token := r.FormValue("issue_submission")
	state := ""
	_ = a.s.db.QueryRow(`SELECT state FROM issue_submissions WHERE token=?`, token).Scan(&state)
	if state == "uncertain" {
		err = &issueCreateError{Uncertain: true, Message: "Issue creation may have completed. Refresh issues / check repository, then explicitly link the issue or create an unlinked card. Creation will not be retried."}
	}
	controls := `<div id="card-issue-recovery" data-state="` + html.EscapeString(state) + `" role="alert"><p>` + html.EscapeString(err.Error()) + `</p>`
	if state != "" && state != "completed" && state != "created" {
		controls += `<label>Recovery action <select name="issue_recovery"><option value="">No recovery selected</option><option value="link">Link existing issue</option><option value="unlinked">Create unlinked card</option>`
		if state == "rejected" {
			controls += `<option value="retry">Retry rejected creation (new submission)</option>`
		}
		controls += `</select></label><label>Same-repository issue URL <input name="recovery_url" type="url" placeholder="https://host/owner/repo/issues/123"></label><p>Refresh issues / check repository before recovering. Select an issue above or paste its URL. Linking/unlinked recovery uses the original submitted draft. A rejected retry uses your corrected fields.</p>`
	}
	controls += `</div>`
	if datastarRequest(r) {
		stream := newSSEWriter(w)
		if validSubmissionToken(token) {
			if stream.write(elementsFrame(`<input id="card-issue-submission" type="hidden" name="issue_submission" value="`+html.EscapeString(token)+`">`)) != nil {
				return
			}
		}
		if stream.write(elementsFrame(controls)) != nil {
			return
		}
		_ = stream.write(elementsFrame(`<div id="notice" class="notice" role="alert">` + html.EscapeString(err.Error()) + `</div>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	// Native fallback retains escaped fields and the original reservation token.
	form := `<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"><title>New card recovery</title><link rel="stylesheet" href="/assets/style.css"></head><body><main><h1>New card</h1><form method="post" action="/cards">`
	for _, name := range []string{"_csrf", "project_id", "issue_submission", "view", "project"} {
		form += `<input type="hidden" name="` + name + `" value="` + html.EscapeString(r.FormValue(name)) + `">`
	}
	form += `<label>Title <input name="title" required value="` + html.EscapeString(r.FormValue("title")) + `"></label><label>Description <textarea name="description">` + html.EscapeString(r.FormValue("description")) + `</textarea></label>`
	checked := ""
	if r.FormValue("create_issue") != "" {
		checked = " checked"
	}
	if state == "" || state == "created" {
		form += `<label><input type="checkbox" name="create_issue" value="yes"` + checked + `>Create issue</label>`
	}
	form += controls + `<button type="submit">Create card</button><a href="/board">Cancel</a></form></main></body></html>`
	_, _ = w.Write([]byte(strings.TrimSpace(form)))
}
