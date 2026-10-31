package sim

import "net/http"

// handleMessages serves the Anthropic Messages format. Implemented with M6.
func (in *Instance) handleMessages(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 501, map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "not implemented"}})
}
