package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"m365-copilot2api/internal/chathub"
)

func TestClientIPTrustsProxyHeadersOnlyFromLoopback(t *testing.T) {
	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.RemoteAddr = "127.0.0.1:4321"
	proxied.Header.Set("CF-Connecting-IP", "203.0.113.9")
	proxied.Header.Set("X-Forwarded-For", "198.51.100.8")
	if got := clientIP(proxied); got != "203.0.113.9" {
		t.Fatalf("proxied client IP = %q, want Cloudflare address", got)
	}

	direct := httptest.NewRequest("GET", "/", nil)
	direct.RemoteAddr = "198.51.100.10:9876"
	direct.Header.Set("CF-Connecting-IP", "203.0.113.99")
	direct.Header.Set("X-Forwarded-For", "203.0.113.98")
	if got := clientIP(direct); got != "198.51.100.10" {
		t.Fatalf("direct client IP = %q, proxy headers were trusted", got)
	}
}

func TestProxyForUsageRemovesCredentialsAndPaths(t *testing.T) {
	rawProxy := "http://" + "user" + ":" + "secret" + "@127.0.0.1:8080/private?q=token"
	got := proxyForUsage(rawProxy)
	if got != "http://127.0.0.1:8080" {
		t.Fatalf("proxyForUsage() = %q", got)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "private") {
		t.Fatalf("sanitized proxy leaked credentials or path: %q", got)
	}
	if got := proxyForUsage("user:secret@proxy.invalid"); got != "configured" {
		t.Fatalf("malformed proxy = %q, want opaque configured marker", got)
	}
}

func TestUsageTraceAggregatesRequestMetrics(t *testing.T) {
	trace := &usageTrace{startedAt: time.Now().Add(-50 * time.Millisecond)}
	trace.setAccount("account-1")
	trace.addQueue(12 * time.Millisecond)
	trace.observeResult(chathub.Result{ConnectMs: 34, RequestID: "upstream-1", LastDeltaMs: 45, Incomplete: true, Text: "partial output", FailureStage: "read_timeout", UpstreamCloseCode: 1006}, 80*time.Millisecond, true, true)
	trace.markFirstToken()
	trace.setToolCalls(2)

	got := trace.snapshot()
	if got.AccountID != "account-1" || got.QueueMs != 12 || got.ConnectMs != 34 {
		t.Fatalf("trace routing metrics = %#v", got)
	}
	if got.ToolPlanningMs != 80 || got.RetryCount != 1 || got.ToolCallCount != 2 {
		t.Fatalf("trace tool metrics = %#v", got)
	}
	if !got.Partial || got.UpstreamRequestID != "upstream-1" || got.InterruptedRequestID != "upstream-1" || got.LastDeltaMs != 45 || got.FailureStage != "read_timeout" || got.UpstreamCloseCode != 1006 || got.PartialOutputTokens == 0 {
		t.Fatalf("trace interruption metrics = %#v", got)
	}
	trace.markStreamRecovered("upstream-2", 20)
	recovered := trace.snapshot()
	if recovered.Partial || !recovered.StreamRecovered || recovered.UpstreamRequestID != "upstream-2" || recovered.InterruptedRequestID != "upstream-1" || recovered.FailureStage != "read_timeout" || recovered.UpstreamCloseCode != 1006 {
		t.Fatalf("trace recovery metrics = %#v", recovered)
	}
	if got.TTFTMs < 40 {
		t.Fatalf("TTFT = %dms, want request-relative latency", got.TTFTMs)
	}
}
