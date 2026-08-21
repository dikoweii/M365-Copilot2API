package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWritePreStreamUpstreamErrorPreservesRateLimitStatus(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	trace := &usageTrace{}

	writePreStreamUpstreamError(w, r, trace, &UpstreamHTTPError{Status: http.StatusTooManyRequests, RetryAfter: 90})

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After=%q want 90", got)
	}
	if got := trace.snapshot().FailureStatus; got != http.StatusTooManyRequests {
		t.Fatalf("trace status=%d want 429", got)
	}
}

func TestWritePreStreamUpstreamErrorStopsAfterClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	trace := &usageTrace{}

	writePreStreamUpstreamError(w, r, trace, context.Canceled)

	if w.Body.Len() != 0 {
		t.Fatalf("wrote %d bytes after client cancellation", w.Body.Len())
	}
	snapshot := trace.snapshot()
	if snapshot.FailureStatus != statusClientClosedRequest || snapshot.FailureType != "client_cancelled" {
		t.Fatalf("trace failure=%d/%q want 499/client_cancelled", snapshot.FailureStatus, snapshot.FailureType)
	}
}

func TestTraceCompletionStatus(t *testing.T) {
	if got := traceCompletionStatus(0, context.Canceled); got != statusClientClosedRequest {
		t.Fatalf("cancelled status=%d want 499", got)
	}
	if got := traceCompletionStatus(0, nil); got != http.StatusOK {
		t.Fatalf("empty status=%d want 200", got)
	}
	if got := traceCompletionStatus(http.StatusTooManyRequests, context.Canceled); got != http.StatusTooManyRequests {
		t.Fatalf("written status=%d want 429", got)
	}
}
