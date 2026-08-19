package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
