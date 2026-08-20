package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestResponsesAdapterRecognizesOpenAIStreamErrorShape(t *testing.T) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(`{"error":{"message":"upstream disconnected","code":"stream_interrupted","partial":true}}`), &chunk); err != nil {
		t.Fatal(err)
	}
	failure, ok := chunk["error"].(map[string]any)
	if !ok {
		t.Fatalf("error frame not recognized: %#v", chunk)
	}
	if failure["code"] != "stream_interrupted" || failure["message"] != "upstream disconnected" {
		t.Fatalf("unexpected failure: %#v", failure)
	}
}

func TestResponsesStreamFailurePreservesDiagnosticsAndStatus(t *testing.T) {
	failure := map[string]any{
		"code":                "authentication_error",
		"message":             "account refresh required",
		"partial":             true,
		"upstream_request_id": "upstream-1",
		"failure_stage":       "websocket_close",
		"upstream_close_code": float64(1006),
	}
	if got := responsesStreamFailureStatus(failure); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	body := responsesStreamFailureBody(failure)
	for _, key := range []string{"partial", "upstream_request_id", "failure_stage", "upstream_close_code"} {
		if body[key] != failure[key] {
			t.Fatalf("diagnostic %q was not preserved: %#v", key, body)
		}
	}
}

func TestResponsesStreamFailureMapsRateLimit(t *testing.T) {
	if got := responsesStreamFailureStatus(map[string]any{"code": "rate_limit"}); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
	}
}
