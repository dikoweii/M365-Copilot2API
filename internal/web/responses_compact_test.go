package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompactionCapsuleRoundTrip(t *testing.T) {
	want := "goal: continue from the first unfinished task\nverified: tests passed"
	capsule, err := encodeCompactionSummary(want)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(capsule, compactionCapsulePrefix) || strings.Contains(capsule, want) {
		t.Fatalf("unexpected capsule %q", capsule)
	}
	got, err := decodeCompactionSummary(capsule)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("decoded summary = %q, want %q", got, want)
	}
}

func TestResponsesCompactionItemToOpenAI(t *testing.T) {
	capsule, err := encodeCompactionSummary("continue from chapter six")
	if err != nil {
		t.Fatal(err)
	}
	r := responsesRequest{Input: []any{
		map[string]any{"type": "compaction", "encrypted_content": capsule},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
	}}
	o, err := r.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Messages) != 2 || o.Messages[0].Role != "system" || !strings.Contains(contentToString(o.Messages[0].Content), "chapter six") {
		t.Fatalf("compaction was not restored: %#v", o.Messages)
	}
}

func TestResponsesCompactRejectsUnsupportedMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/compact", nil)
	w := httptest.NewRecorder()
	new(Server).responsesCompact(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestTransientModelRequestMarker(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	if isTransientModelRequest(req) || !isTransientModelRequest(withTransientModelRequest(req)) {
		t.Fatal("transient request marker was not isolated to request context")
	}
}

func TestIsolateTransientModelRequestClearsConversationIdentity(t *testing.T) {
	body := oaiReq{
		ConversationID:  "conversation",
		ConversationIDC: "conversation-compat",
		SessionID:       "session",
		SessionIDC:      "session-compat",
		SessionKey:      "session-key",
		User:            "user-key",
		AccountID:       "account",
	}
	isolateTransientModelRequest(&body)
	if body.ConversationID != "" || body.ConversationIDC != "" || body.SessionID != "" || body.SessionIDC != "" || body.SessionKey != "" || body.User != "" {
		t.Fatalf("transient identity was not cleared: %#v", body)
	}
	if body.AccountID != "account" {
		t.Fatalf("account routing must be preserved, got %q", body.AccountID)
	}
}

func TestNormalizeCompactionMessagesAcceptsIncompleteToolHistory(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "inspect the project"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"README.md"}`}}}},
		{Role: "assistant", Content: "the connection ended before the result was recorded"},
		{Role: "tool", ToolCallID: "orphaned_call", Content: "historical result"},
	}

	normalized := normalizeCompactionMessages(messages)
	if err := validateToolConversation(normalized); err != nil {
		t.Fatalf("normalized compaction history failed validation: %v", err)
	}
	if len(normalized[1].ToolCalls) != 0 || normalized[3].Role == "tool" || normalized[3].ToolCallID != "" {
		t.Fatalf("tool protocol metadata survived normalization: %#v", normalized)
	}
	if !strings.Contains(contentToString(normalized[1].Content), "results may be missing") || !strings.Contains(contentToString(normalized[3].Content), "orphaned_call") {
		t.Fatalf("tool history was not preserved as transcript text: %#v", normalized)
	}
}

func TestBoundedCompactionTranscriptKeepsHeadTailAndUTF8(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "HEAD-IMPORTANT-规则"},
		{Role: "user", Content: strings.Repeat("middle-内容-", 200)},
		{Role: "assistant", Content: "TAIL-IMPORTANT-结论"},
	}
	transcript, sourceTokens, truncated := boundedCompactionTranscript(messages, 80)
	if !truncated || sourceTokens <= 80 {
		t.Fatalf("oversized transcript was not bounded: tokens=%d truncated=%v", sourceTokens, truncated)
	}
	if !strings.Contains(transcript, "HEAD-IMPORTANT") || !strings.Contains(transcript, "TAIL-IMPORTANT") || !strings.Contains(transcript, "middle history omitted") {
		t.Fatalf("bounded transcript lost anchors: %q", transcript)
	}
	if !utf8.ValidString(transcript) {
		t.Fatalf("bounded transcript is not valid UTF-8: %q", transcript)
	}
	if EstimateTokens(transcript) > 80 {
		t.Fatalf("bounded transcript tokens=%d, want <= 80", EstimateTokens(transcript))
	}
}
