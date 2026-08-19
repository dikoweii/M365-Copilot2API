package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

type usageTraceKey struct{}
type toolPlanningKey struct{}
type retryAttemptKey struct{}
type internalUsageAdapterKey struct{}

type usageTrace struct {
	mu                   sync.Mutex
	startedAt            time.Time
	queueMs              int64
	connectMs            int64
	ttftMs               int64
	toolPlanningMs       int64
	retryCount           int
	toolCallCount        int
	partial              bool
	streamRecovered      bool
	partialOutputTokens  int64
	upstreamRequestID    string
	interruptedRequestID string
	lastDeltaMs          int64
	failureStage         string
	upstreamCloseCode    int
	recorded             bool
	failureStatus        int
	failureType          string
	failureMessage       string
	accountID            string
}

type usageTraceSnapshot struct {
	QueueMs              int64
	ConnectMs            int64
	TTFTMs               int64
	ToolPlanningMs       int64
	RetryCount           int
	ToolCallCount        int
	Partial              bool
	StreamRecovered      bool
	PartialOutputTokens  int64
	UpstreamRequestID    string
	InterruptedRequestID string
	LastDeltaMs          int64
	FailureStage         string
	UpstreamCloseCode    int
	Recorded             bool
	FailureStatus        int
	FailureType          string
	FailureMessage       string
	AccountID            string
}

func ensureUsageTrace(r *http.Request, startedAt time.Time) (*http.Request, *usageTrace) {
	if existing := usageTraceFrom(r.Context()); existing != nil {
		return r, existing
	}
	trace := &usageTrace{startedAt: startedAt}
	return r.WithContext(context.WithValue(r.Context(), usageTraceKey{}, trace)), trace
}

func usageTraceFrom(ctx context.Context) *usageTrace {
	trace, _ := ctx.Value(usageTraceKey{}).(*usageTrace)
	return trace
}

func withToolPlanning(ctx context.Context) context.Context {
	return context.WithValue(ctx, toolPlanningKey{}, true)
}

func isToolPlanning(ctx context.Context) bool {
	value, _ := ctx.Value(toolPlanningKey{}).(bool)
	return value
}

func withRetryAttempt(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryAttemptKey{}, true)
}

func isRetryAttempt(ctx context.Context) bool {
	value, _ := ctx.Value(retryAttemptKey{}).(bool)
	return value
}

func withInternalUsageAdapter(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalUsageAdapterKey{}, true)
}

func isInternalUsageAdapter(r *http.Request) bool {
	value, _ := r.Context().Value(internalUsageAdapterKey{}).(bool)
	return value
}

func (t *usageTrace) addQueue(elapsed time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.queueMs += elapsed.Milliseconds()
	t.mu.Unlock()
}

func (t *usageTrace) setAccount(accountID string) {
	if t == nil || accountID == "" {
		return
	}
	t.mu.Lock()
	t.accountID = accountID
	t.mu.Unlock()
}

func (t *usageTrace) observeResult(result chathub.Result, elapsed time.Duration, planning, retry bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if result.ConnectMs > 0 {
		t.connectMs += result.ConnectMs
	}
	if planning {
		t.toolPlanningMs += elapsed.Milliseconds()
	}
	if retry {
		t.retryCount++
	}
	if result.RequestID != "" {
		t.upstreamRequestID = result.RequestID
	}
	if result.LastDeltaMs > 0 {
		t.lastDeltaMs = result.LastDeltaMs
	}
	if result.Incomplete {
		t.partial = true
		t.partialOutputTokens = EstimateTokens(result.Text)
		t.interruptedRequestID = result.RequestID
		t.failureStage = result.FailureStage
		t.upstreamCloseCode = result.UpstreamCloseCode
	}
	t.mu.Unlock()
}

func (t *usageTrace) markFirstToken() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.ttftMs == 0 {
		t.ttftMs = time.Since(t.startedAt).Milliseconds()
		if t.ttftMs < 1 {
			t.ttftMs = 1
		}
	}
	t.mu.Unlock()
}

func (t *usageTrace) setToolCalls(count int) {
	if t == nil || count <= 0 {
		return
	}
	t.mu.Lock()
	if count > t.toolCallCount {
		t.toolCallCount = count
	}
	t.mu.Unlock()
}

func (t *usageTrace) markStreamRecovered(upstreamRequestID string, lastDeltaMs int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.partial = false
	t.streamRecovered = true
	t.partialOutputTokens = 0
	if upstreamRequestID != "" {
		t.upstreamRequestID = upstreamRequestID
	}
	if lastDeltaMs > 0 {
		t.lastDeltaMs = lastDeltaMs
	}
	t.mu.Unlock()
}

func (t *usageTrace) markStreamPartial(result chathub.Result) {
	if t == nil || !result.Incomplete {
		return
	}
	t.mu.Lock()
	t.partial = true
	t.streamRecovered = false
	t.partialOutputTokens = EstimateTokens(result.Text)
	t.upstreamRequestID = result.RequestID
	t.interruptedRequestID = result.RequestID
	t.lastDeltaMs = result.LastDeltaMs
	t.failureStage = result.FailureStage
	t.upstreamCloseCode = result.UpstreamCloseCode
	t.mu.Unlock()
}

func (t *usageTrace) fail(status int, errorType, message string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.failureStatus = status
	t.failureType = strings.TrimSpace(errorType)
	t.failureMessage = strings.TrimSpace(message)
	t.mu.Unlock()
}

func (t *usageTrace) markRecorded() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.recorded = true
	t.mu.Unlock()
}

func (t *usageTrace) snapshot() usageTraceSnapshot {
	if t == nil {
		return usageTraceSnapshot{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return usageTraceSnapshot{
		QueueMs: t.queueMs, ConnectMs: t.connectMs, TTFTMs: t.ttftMs,
		ToolPlanningMs: t.toolPlanningMs, RetryCount: t.retryCount,
		ToolCallCount: t.toolCallCount, Partial: t.partial, StreamRecovered: t.streamRecovered,
		PartialOutputTokens: t.partialOutputTokens, UpstreamRequestID: t.upstreamRequestID,
		InterruptedRequestID: t.interruptedRequestID,
		LastDeltaMs:          t.lastDeltaMs, FailureStage: t.failureStage,
		UpstreamCloseCode: t.upstreamCloseCode, Recorded: t.recorded,
		FailureStatus: t.failureStatus, FailureType: t.failureType,
		FailureMessage: t.failureMessage, AccountID: t.accountID,
	}
}

func (s *Server) recordUsage(r *http.Request, acc auth.AccountToken, rec UsageRecord) {
	if s == nil || s.usage == nil || r == nil || isInternalUsageAdapter(r) {
		return
	}
	metrics := usageTraceSnapshot{}
	if trace := usageTraceFrom(r.Context()); trace != nil {
		metrics = trace.snapshot()
		if acc.ID == "" && metrics.AccountID != "" && s.tokens != nil {
			if selected, ok := s.tokens.Get(metrics.AccountID); ok {
				acc = selected
			}
		}
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}
	if rec.RequestID == "" {
		rec.RequestID = requestIDFrom(r)
	}
	if rec.APIKeyPrefix == "" {
		rec.APIKeyPrefix = extractAPIKey(r)
	}
	if rec.AccountEmail == "" {
		rec.AccountEmail = acc.Email
	}
	if rec.ClientIP == "" {
		rec.ClientIP = clientIP(r)
	}
	if trustedProxyPeer(r) {
		rec.CFRay = strings.TrimSpace(r.Header.Get("CF-Ray"))
		rec.ClientCountry = strings.TrimSpace(r.Header.Get("CF-IPCountry"))
	}
	if rec.Proxy == "" {
		rec.Proxy = proxyForUsage(acc.BoundProxy)
	}
	if rec.UsageSource == "" {
		rec.UsageSource = "gateway_estimate"
	}
	if trace := usageTraceFrom(r.Context()); trace != nil {
		if rec.TTFTMs == 0 {
			rec.TTFTMs = metrics.TTFTMs
		}
		if rec.QueueMs == 0 {
			rec.QueueMs = metrics.QueueMs
		}
		if rec.ConnectMs == 0 {
			rec.ConnectMs = metrics.ConnectMs
		}
		if rec.ToolPlanningMs == 0 {
			rec.ToolPlanningMs = metrics.ToolPlanningMs
		}
		if rec.RetryCount == 0 {
			rec.RetryCount = metrics.RetryCount
		}
		if rec.ToolCallCount == 0 {
			rec.ToolCallCount = metrics.ToolCallCount
		}
		if rec.OutputTokens == 0 && metrics.PartialOutputTokens > 0 {
			rec.OutputTokens = metrics.PartialOutputTokens
		}
		if !rec.Partial {
			rec.Partial = metrics.Partial
		}
		if !rec.StreamRecovered {
			rec.StreamRecovered = metrics.StreamRecovered
		}
		if rec.UpstreamRequestID == "" {
			rec.UpstreamRequestID = metrics.UpstreamRequestID
		}
		if rec.InterruptedRequestID == "" {
			rec.InterruptedRequestID = metrics.InterruptedRequestID
		}
		if rec.LastDeltaMs == 0 {
			rec.LastDeltaMs = metrics.LastDeltaMs
		}
		if rec.FailureStage == "" {
			rec.FailureStage = metrics.FailureStage
		}
		if rec.UpstreamCloseCode == 0 {
			rec.UpstreamCloseCode = metrics.UpstreamCloseCode
		}
		if metrics.FailureStatus > 0 {
			rec.Status = metrics.FailureStatus
			rec.ErrorType = firstNonEmpty(rec.ErrorType, metrics.FailureType)
			rec.ErrorMessage = firstNonEmpty(rec.ErrorMessage, metrics.FailureMessage)
		}
		trace.markRecorded()
	}
	s.usage.record(rec)
}

func writeToolResponseTracked(r *http.Request, w http.ResponseWriter, id, model string, stream bool, calls []detectedToolCall, res chathub.Result) error {
	if trace := usageTraceFrom(r.Context()); trace != nil {
		trace.setToolCalls(len(calls))
		trace.markFirstToken()
	}
	return writeToolResponse(w, id, model, stream, calls, res)
}

func trustedProxyPeer(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func proxyForUsage(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "configured"
	}
	if u.Scheme == "" {
		return u.Host
	}
	return fmt.Sprintf("%s://%s", strings.ToLower(u.Scheme), u.Host)
}

type usageResponseWriter struct {
	http.ResponseWriter
	status       int
	wroteHeader  bool
	errorPreview strings.Builder
}

func (s *Server) observeUsageResponse(w http.ResponseWriter, r *http.Request, startedAt time.Time, fallback func() (auth.AccountToken, UsageRecord)) (http.ResponseWriter, func()) {
	observer := &usageResponseWriter{ResponseWriter: w}
	finish := func() {
		metrics := usageTraceSnapshot{}
		if trace := usageTraceFrom(r.Context()); trace != nil {
			metrics = trace.snapshot()
		}
		if metrics.Recorded || isInternalUsageAdapter(r) {
			return
		}
		acc, rec := fallback()
		status := observer.Status()
		if metrics.FailureStatus > 0 {
			status = metrics.FailureStatus
		}
		rec.Status = status
		if rec.DurationMs == 0 {
			rec.DurationMs = time.Since(startedAt).Milliseconds()
		}
		if status >= http.StatusBadRequest {
			rec.ErrorType = firstNonEmpty(rec.ErrorType, metrics.FailureType, usageErrorType(status))
			rec.ErrorMessage = firstNonEmpty(rec.ErrorMessage, metrics.FailureMessage, strings.TrimSpace(observer.errorPreview.String()))
			rec.ErrorMessage = sanitizePublicInternalText(rec.ErrorMessage)
		}
		s.recordUsage(r, acc, rec)
	}
	return observer, finish
}

func (w *usageResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *usageResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.status >= http.StatusBadRequest && w.errorPreview.Len() < 1024 {
		remaining := 1024 - w.errorPreview.Len()
		preview := p
		if len(preview) > remaining {
			preview = preview[:remaining]
		}
		_, _ = w.errorPreview.Write(preview)
	}
	return w.ResponseWriter.Write(p)
}

func (w *usageResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *usageResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *usageResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func usageErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication"
	case status >= 500:
		return "upstream_or_server"
	case status >= 400:
		return "client_request"
	default:
		return ""
	}
}
