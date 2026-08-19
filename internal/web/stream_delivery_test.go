package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

type rejectedStreamWriter struct {
	header http.Header
}

func (w *rejectedStreamWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *rejectedStreamWriter) Write([]byte) (int, error) {
	return 0, errors.New("write rejected")
}

func (w *rejectedStreamWriter) WriteHeader(int) {}

func (w *rejectedStreamWriter) Flush() {}

func TestOpenAIStreamEmitterWritesReasoningAndMarksTTFTAfterWrite(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req, trace := ensureUsageTrace(req, time.Now().Add(-25*time.Millisecond))
	recorder := httptest.NewRecorder()
	delivery := &streamDeliveryState{}
	emitter := newOpenAIStreamEmitter(req, recorder, recorder, "chatcmpl_test", "gpt-5.6-reasoning", trace, delivery)

	if err := emitter.writeDelta(map[string]any{"reasoning_content": ""}); err != nil {
		t.Fatal(err)
	}
	if recorder.Body.Len() != 0 || trace.snapshot().TTFTMs != 0 || !delivery.canRetry() {
		t.Fatalf("empty delta became visible: body=%q trace=%#v retry=%t", recorder.Body.String(), trace.snapshot(), delivery.canRetry())
	}

	if err := emitter.writeDelta(map[string]any{"reasoning_content": "thinking"}); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"reasoning_content":"thinking"`) || !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("reasoning delta missing from SSE: %s", body)
	}
	if delivery.canRetry() || trace.snapshot().TTFTMs == 0 {
		t.Fatalf("visible reasoning did not lock retry/TTFT: trace=%#v retry=%t", trace.snapshot(), delivery.canRetry())
	}
}

func TestOpenAIStreamEmitterDoesNotMarkTTFTWhenWriteFails(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req, trace := ensureUsageTrace(req, time.Now().Add(-25*time.Millisecond))
	w := &rejectedStreamWriter{}
	delivery := &streamDeliveryState{}
	emitter := newOpenAIStreamEmitter(req, w, w, "chatcmpl_test", "gpt-5.6-reasoning", trace, delivery)

	if err := emitter.writeDelta(map[string]any{"content": "hello"}); err == nil {
		t.Fatal("expected write error")
	}
	if trace.snapshot().TTFTMs != 0 || !delivery.canRetry() {
		t.Fatalf("failed write marked a visible token: trace=%#v retry=%t", trace.snapshot(), delivery.canRetry())
	}
}

func TestVisibleStreamEventsDisableAccountRetry(t *testing.T) {
	t.Run("progress", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", nil)
		req, trace := ensureUsageTrace(req, time.Now().Add(-25*time.Millisecond))
		recorder := httptest.NewRecorder()
		delivery := &streamDeliveryState{}
		if err := writeVisibleSSE(req, recorder, recorder, "progress", map[string]any{"type": "progress", "text": "searching"}, delivery, trace); err != nil {
			t.Fatal(err)
		}
		if delivery.canRetry() || trace.snapshot().TTFTMs == 0 || !strings.Contains(recorder.Body.String(), "event: progress") {
			t.Fatalf("progress event remained retryable: body=%s trace=%#v", recorder.Body.String(), trace.snapshot())
		}
	})

	t.Run("tool", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req, trace := ensureUsageTrace(req, time.Now().Add(-25*time.Millisecond))
		recorder := httptest.NewRecorder()
		delivery := &streamDeliveryState{}
		calls := []detectedToolCall{{ID: "call_test", Name: "lookup", Arguments: []byte(`{"query":"x"}`)}}
		if err := writeVisibleToolResponse(req, recorder, delivery, "chatcmpl_test", "gpt-5.6", calls, chathub.Result{}); err != nil {
			t.Fatal(err)
		}
		if delivery.canRetry() || trace.snapshot().TTFTMs == 0 || !strings.Contains(recorder.Body.String(), `"tool_calls"`) {
			t.Fatalf("tool event remained retryable: body=%s trace=%#v", recorder.Body.String(), trace.snapshot())
		}
	})
}

func TestRequiredStreamToolFallbackStopsAfterRouterAndRepair(t *testing.T) {
	tools := []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name": "lookup",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"query": map[string]any{"type": "string"}},
				"required":   []any{"query"},
			},
		},
	}}
	calls := 0
	planner := func(context.Context, chathub.Request) (chathub.Result, error) {
		calls++
		if calls == 1 {
			return chathub.Result{Text: "not a tool decision"}, nil
		}
		return chathub.Result{Text: `{"calls":[]}`}, nil
	}

	got, _, err := planRequiredStreamToolCall(context.Background(), "route", "Gpt_5_6_Reasoning", nil, tools, "required", agentLedger{}, planner)
	if !errors.Is(err, errRequiredStreamToolCall) || len(got) != 0 {
		t.Fatalf("required fallback result calls=%#v err=%v", got, err)
	}
	if calls != 2 {
		t.Fatalf("router/repair calls=%d, want exactly 2", calls)
	}

	recorder := httptest.NewRecorder()
	writeOpenAIStreamError(context.Background(), recorder, recorder, "model did not select a required tool after one constrained routing attempt", "tool_protocol_error")
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"tool_protocol_error"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("tool protocol error missing from SSE: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("required tool failure was serialized as a normal stop: %s", body)
	}
}

func TestStreamErrorCodeMatchesUpstreamFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "rate_limit", err: &UpstreamHTTPError{Status: http.StatusTooManyRequests}, want: "rate_limit"},
		{name: "authentication", err: &UpstreamHTTPError{Status: http.StatusUnauthorized}, want: "authentication_error"},
		{name: "interrupted", err: &chathub.StreamInterruptedError{Stage: "websocket_close"}, want: "stream_interrupted"},
		{name: "upstream", err: fmt.Errorf("websocket closed"), want: "upstream_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamErrorCode(tc.err); got != tc.want {
				t.Fatalf("streamErrorCode(%v)=%q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestOpenAIStreamFailureIncludesPartialDiagnostics(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeOpenAIStreamFailure(context.Background(), recorder, recorder, "stream ended", "stream_interrupted", chathub.Result{
		Incomplete: true, RequestID: "upstream-1", FailureStage: "websocket_close", UpstreamCloseCode: 1006, LastDeltaMs: 1234,
	})
	body := recorder.Body.String()
	for _, want := range []string{`"partial":true`, `"upstream_request_id":"upstream-1"`, `"failure_stage":"websocket_close"`, `"upstream_close_code":1006`, `"last_delta_ms":1234`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
}
