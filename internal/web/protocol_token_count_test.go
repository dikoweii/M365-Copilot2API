package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicCountTokensIncludesSystemMessagesToolsAndChoice(t *testing.T) {
	base, _ := countAnthropicTokensForTest(t, `{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"weather"}]
	}`)
	withSystem, _ := countAnthropicTokensForTest(t, `{
		"model":"gpt-5.5",
		"system":"Answer briefly.",
		"messages":[{"role":"user","content":"weather"}]
	}`)
	withTools, _ := countAnthropicTokensForTest(t, `{
		"model":"gpt-5.5",
		"system":"Answer briefly.",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
	}`)
	withChoice, rr := countAnthropicTokensForTest(t, `{
		"model":"gpt-5.5",
		"system":"Answer briefly.",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{"name":"weather","description":"Get weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"weather"}
	}`)

	if !(base < withSystem && withSystem < withTools && withTools < withChoice) {
		t.Fatalf("expected every request field to add tokens: base=%d system=%d tools=%d choice=%d", base, withSystem, withTools, withChoice)
	}
	if got := rr.Header().Get(anthropicTokenEstimateHeader); got != "true" {
		t.Fatalf("estimate header = %q", got)
	}
	if got := rr.Header().Get(anthropicTokenEstimateSourceHeader); got != usageSourceTiktoken {
		t.Fatalf("estimate source = %q", got)
	}
}

func TestAnthropicCountTokensKeepsSDKResponseShape(t *testing.T) {
	_, rr := countAnthropicTokensForTest(t, `{"model":"claude-sonnet","messages":[{"role":"user","content":"hello"}]}`)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	if len(body) != 1 || body["input_tokens"] == nil {
		t.Fatalf("unexpected count_tokens body: %#v", body)
	}
	if rr.Header().Get(anthropicTokenEstimateHeader) != "true" || rr.Header().Get(anthropicTokenEstimateSourceHeader) == "" {
		t.Fatalf("missing estimate metadata headers: %#v", rr.Header())
	}
}

func TestAnthropicCountTokensRejectsNonPost(t *testing.T) {
	rr := httptest.NewRecorder()
	(&Server{}).anthropicCountTokens(rr, httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAnthropicCountTokensIsProtectedWhenRegisteredUnderV1(t *testing.T) {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages/count_tokens", s.anthropicCountTokens)
	rr := httptest.NewRecorder()
	s.adminMiddleware(mux).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-5.5","messages":[]}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func countAnthropicTokensForTest(t *testing.T, payload string) (int, *httptest.ResponseRecorder) {
	t.Helper()
	rr := httptest.NewRecorder()
	(&Server{}).anthropicCountTokens(rr, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(payload)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response JSON: %v", err)
	}
	return body.InputTokens, rr
}
