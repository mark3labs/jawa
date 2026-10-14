package orchestrator

import "net/http"

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
	case "/cards/delete":
		return "delete"
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

func dialogErrorID(name, operation string) string {
	if len(operation) == 32 {
		valid := true
		for _, c := range operation {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				valid = false
				break
			}
		}
		if valid {
			return name + "-error-" + operation
		}
	}
	return name + "-error"
}
