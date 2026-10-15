package orchestrator

import (
	"bytes"
	"net/http"

	"github.com/a-h/templ"
)

// Failure patches only recovery controls, never draft fields or the dialog.
func (a *app) issueCardError(w http.ResponseWriter, r *http.Request, err error) {
	token := r.FormValue("issue_submission")
	state := ""
	_ = a.s.db.QueryRow(`SELECT state FROM issue_submissions WHERE token=?`, token).Scan(&state)
	if state == "uncertain" {
		err = &issueCreateError{Uncertain: true, Message: "Issue creation may have completed. Refresh issues / check repository, then explicitly link the issue or create an unlinked card. Creation will not be retried."}
	}
	render := func(component templ.Component) string {
		var buf bytes.Buffer
		_ = component.Render(r.Context(), &buf)
		return buf.String()
	}
	controls := render(issueRecoveryControls(state, err.Error()))
	if datastarRequest(r) {
		stream := newSSEWriter(w)
		if validSubmissionToken(token) {
			if stream.write(elementsFrame(render(issueSubmissionToken(token)))) != nil {
				return
			}
		}
		if stream.write(elementsFrameMode("replace", "", controls)) != nil {
			return
		}
		if stream.write(elementsFrameMode("replace", "", render(cardIssueField(nil, "Refresh issues / check repository before recovering.", state)))) != nil {
			return
		}
		_ = stream.write(elementsFrame(render(dialogError("card", r.FormValue("dialog_operation"), err.Error()))))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusConflict)
	_ = issueRecoveryPage(r, state, err.Error()).Render(r.Context(), w)
}
