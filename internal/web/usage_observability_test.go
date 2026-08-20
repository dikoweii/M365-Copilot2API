package web

import (
	"testing"
	"time"
)

func TestUsageSnapshotObservabilityPercentiles(t *testing.T) {
	now := time.Now()
	records := make([]UsageRecord, 0, 100)
	for i := 1; i <= 100; i++ {
		status := 200
		if i > 90 {
			status = 500
		}
		ttft := int64(0)
		if i <= 4 {
			ttft = []int64{10, 20, 30, 100}[i-1]
		}
		records = append(records, UsageRecord{
			Time:       now,
			DurationMs: int64(i),
			TTFTMs:     ttft,
			Status:     status,
		})
	}

	stats := (&usageLog{records: records}).snapshot(1)
	summary := stats["summary"].(map[string]any)
	assertInt64(t, summary, "successes", 90)
	assertInt64(t, summary, "errors", 10)
	if got := summary["success_rate"].(float64); got != 90 {
		t.Fatalf("success_rate = %v, want 90", got)
	}
	assertInt64(t, summary, "p50_ms", 50)
	assertInt64(t, summary, "p95_ms", 95)
	assertInt64(t, summary, "p99_ms", 99)
	assertInt64(t, summary, "ttft_avg_ms", 40)
	assertInt64(t, summary, "ttft_p50_ms", 20)
	assertInt64(t, summary, "ttft_p95_ms", 100)

	statuses := stats["statuses"].([]map[string]any)
	assertAggregate(t, statuses, "status", 200, 90, 0)
	assertAggregate(t, statuses, "status", 500, 10, 0)
}

func TestUsageSnapshotDimensionAggregatesIgnoreMissingOptionalFields(t *testing.T) {
	now := time.Now()
	s := &usageLog{records: []UsageRecord{
		{Time: now, AccountEmail: "account-a", InputTokens: 1, Status: 200},
		{Time: now, ClientIP: "192.0.2.1", AccountEmail: "account-a", Proxy: "proxy-a", InputTokens: 2, Status: 200},
		{Time: now, ClientIP: "192.0.2.1", AccountEmail: "account-b", Proxy: "proxy-a", OutputTokens: 3, Status: 429},
		{Time: now, ClientIP: "198.51.100.2", AccountEmail: "account-a", Proxy: "proxy-b", CacheTokens: 4, Status: 500},
	}}

	stats := s.snapshot(1)
	ips := stats["ips"].([]map[string]any)
	accounts := stats["accounts"].([]map[string]any)
	proxies := stats["proxies"].([]map[string]any)

	if len(ips) != 2 {
		t.Fatalf("len(ips) = %d, want 2", len(ips))
	}
	if len(proxies) != 2 {
		t.Fatalf("len(proxies) = %d, want 2", len(proxies))
	}
	assertAggregate(t, ips, "client_ip", "192.0.2.1", 2, 5)
	assertAggregate(t, ips, "client_ip", "198.51.100.2", 1, 4)
	assertAggregate(t, accounts, "account_email", "account-a", 3, 7)
	assertAggregate(t, accounts, "account_email", "account-b", 1, 3)
	assertAggregate(t, proxies, "proxy", "proxy-a", 2, 5)
	assertAggregate(t, proxies, "proxy", "proxy-b", 1, 4)
}

func assertInt64(t *testing.T, values map[string]any, key string, want int64) {
	t.Helper()
	if got := values[key].(int64); got != want {
		t.Fatalf("%s = %d, want %d", key, got, want)
	}
}

func assertAggregate(t *testing.T, aggregates []map[string]any, keyName string, key any, requests, tokens int64) {
	t.Helper()
	for _, aggregate := range aggregates {
		if aggregate[keyName] != key {
			continue
		}
		if got := aggregate["requests"].(int64); got != requests {
			t.Fatalf("%s %v requests = %d, want %d", keyName, key, got, requests)
		}
		if tokens > 0 {
			if got := aggregate["tokens"].(int64); got != tokens {
				t.Fatalf("%s %v tokens = %d, want %d", keyName, key, got, tokens)
			}
		}
		return
	}
	t.Fatalf("%s %v not found in aggregates", keyName, key)
}
