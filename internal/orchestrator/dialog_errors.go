package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// Only server-owned routes select a dialog; the opaque operation suffix is
// validated and never interpreted as a browser-supplied selector.
func mutationDialog(r *http.Request) string {
	if r.Method != http.MethodPost {
		return ""
	}
	switch r.URL.Path {
	case "/projects":
		return "project"
	case "/cards":
		return "card"
	case "/cards/assign":
		return "assign"
	case "/cards/delete":
		return "delete"
	case "/cards/merge":
		return "merge"
	case "/cards/fix":
		return "fix"
	case "/cards/guide":
		// The inline planning-page reply form shares this route; only the
		// guide dialog submits dialog_operation, so inline failures stay on
		// the visible page notice instead of a closed dialog.
		if err := r.ParseForm(); err != nil || !r.PostForm.Has("dialog_operation") {
			return ""
		}
		return "guide"
	case "/cards/cancel":
		return "cancel"
	case "/cards/force-ready":
		return "ready"
	case "/cards/force-stop":
		return "force"
	}
	return ""
}

func newOperationID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validOperation(operation string) bool {
	if len(operation) != 32 {
		return false
	}
	for _, c := range operation {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func dialogErrorID(name, operation string) string {
	if validOperation(operation) {
		return name + "-error-" + operation
	}
	return name + "-error"
}
