package web

import (
	"testing"
	"time"
)

func TestUsageSnapshotSeparatesHistoryFromBilledTokens(t *testing.T) {
	s := &usageLog{records: []UsageRecord{{
		Time:               time.Now(),
		InputTokens:        10,
		OutputTokens:       2,
		CacheTokens:        3,
		HistoryTokens:      100,
		ConversationReused: true,
	}}}

	summary := s.snapshot(1)["summary"].(map[string]any)
	if got := summary["tokens"].(int64); got != 15 {
		t.Fatalf("tokens = %d, want 15", got)
	}
	if got := summary["history"].(int64); got != 100 {
		t.Fatalf("history = %d, want 100", got)
	}
	if got := summary["conversation_reused"].(int64); got != 1 {
		t.Fatalf("conversation_reused = %d, want 1", got)
	}
}
