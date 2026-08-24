package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicErrorEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	writeAnthropicError(rr, 400, "invalid_request_error", "bad json")
	var v map[string]any
	if json.Unmarshal(rr.Body.Bytes(), &v) != nil || v["type"] != "error" {
		t.Fatalf("body=%s", rr.Body.String())
	}
	e, _ := v["error"].(map[string]any)
	if e["type"] != "invalid_request_error" || e["message"] != "bad json" {
		t.Fatalf("body=%s", rr.Body.String())
	}
	if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatal("missing json content type")
	}
}
func TestOpenAIErrorEnvelopeAndExtraction(t *testing.T) {
	rr := httptest.NewRecorder()
	writeResponsesError(rr, 409, "tool_round_limit", "limit reached")
	if got := errorMessage(rr.Body.Bytes(), "fallback"); got != "limit reached" {
		t.Fatalf("message=%q", got)
	}
}

func TestErrorMessageRejectsRawHTMLAndInternalURL(t *testing.T) {
	const fallback = "upstream protocol error"
	if got := errorMessage([]byte(`<html><body>proxy failed at https://internal.example/token</body></html>`), fallback); got != fallback {
		t.Fatalf("raw HTML leaked: %q", got)
	}
	if got := errorMessage([]byte(`{"error":{"message":"dial https://internal.example/private failed"}}`), fallback); got != fallback {
		t.Fatalf("internal URL leaked: %q", got)
	}
	if got := errorMessage([]byte(`{"error":{"message":"tool results missing before assistant message at index 18"}}`), fallback); got != "tool results missing before assistant message at index 18" {
		t.Fatalf("safe actionable message lost: %q", got)
	}
}
func TestMaxToolRoundsConfig(t *testing.T) {
	t.Setenv("M365_MAX_TOOL_ROUNDS", "7")
	if maxToolRounds() != 7 {
		t.Fatal("configured limit ignored")
	}
	t.Setenv("M365_MAX_TOOL_ROUNDS", "9999")
	if maxToolRounds() != 32 {
		t.Fatal("invalid limit accepted")
	}
}
