package web

import (
	"encoding/json"
	"net/http"
)

const (
	anthropicTokenEstimateHeader       = "X-M365-Token-Count-Estimated"
	anthropicTokenEstimateSourceHeader = "X-M365-Token-Count-Source"
)

// anthropicCountTokens estimates the visible Anthropic request context. The
// public response body stays compatible with Anthropic SDKs; estimate metadata
// is exposed through headers instead of additional JSON fields.
func (s *Server) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, "")
	w.Header().Set(anthropicTokenEstimateHeader, "true")
	w.Header().Set(anthropicTokenEstimateSourceHeader, estimate.Source)
	jsonOut(w, map[string]any{"input_tokens": estimate.Values["input_tokens"]})
}
