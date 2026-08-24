package web

import (
	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteRoutedFinalAnswerStreamingProtocol(t *testing.T) {
	s := &Server{}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	body := oaiReq{Model: "gpt-5.6-sol", Stream: true}

	s.writeRoutedFinalAnswer(rr, r, body, "prompt", "DSH_M365_ROUTED_OK", chathub.Result{}, auth.AccountToken{}, time.Now())

	response := rr.Body.String()
	if rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("unexpected content type: %q", rr.Header().Get("Content-Type"))
	}
	if strings.Count(response, "DSH_M365_ROUTED_OK") != 1 {
		t.Fatalf("routed answer missing or duplicated: %s", response)
	}
	if !strings.Contains(response, `"finish_reason":"stop"`) || !strings.Contains(response, "data: [DONE]") {
		t.Fatalf("stream termination is incomplete: %s", response)
	}
}
