package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCompactToolResultKeepsHeadTailAndError(t *testing.T) {
	s := "start\n" + strings.Repeat("progress line\n", 1000) + "ERROR: build failed\nexit code 1"
	got := compactToolResult(s, 800)
	if len(got) > 900 || !strings.Contains(got, "start") || !strings.Contains(got, "ERROR: build failed") || !strings.Contains(got, "exit code 1") || !strings.Contains(got, "truncated") {
		t.Fatalf("bad compact result: %d %q", len(got), got)
	}
}

func TestAgentLedgerDetectsRepeatedFailure(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c1", Content: "exit code 1: failed"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "c2", "type": "function", "function": map[string]any{"name": "run", "arguments": "{\"cmd\":\"build\"}"}}}},
		{Role: "tool", ToolCallID: "c2", Content: "exit code 1: failed"},
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedFailure {
		t.Fatalf("expected repeated failure: %+v", l)
	}
	if !strings.Contains(l.RouterContext(), "change strategy") {
		t.Fatal(l.RouterContext())
	}
}

func TestAgentLedgerEvidenceAndUniqueCallIDs(t *testing.T) {
	a := scopedCallID("run", "{}", 0, "turn-a")
	b := scopedCallID("run", "{}", 0, "turn-b")
	if a == b {
		t.Fatal("call IDs collide across turns")
	}
	l := buildAgentLedger([]oaiMsg{{Role: "assistant", ToolCalls: []map[string]any{{"id": "c1", "type": "function", "function": map[string]any{"name": "create", "arguments": "{}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "created"}})
	if len(l.Completed) != 1 || !strings.Contains(l.RouterContext(), "c1") {
		t.Fatalf("missing evidence: %+v", l)
	}
}

func TestActiveMessagesKeepsToolChainWithCompatibilityUserMessage(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "inspect the current goal"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_goal", "arguments": "{}"}}}},
		{Role: "user", Content: `{"goal":null}`},
		{Role: "tool", ToolCallID: "call_1", Content: `{"goal":null}`},
	}
	active := activeMessages(messages)
	if len(active) != 4 || active[0].Role != "user" || len(active[1].ToolCalls) != 1 {
		t.Fatalf("tool chain was truncated: %#v", active)
	}
	ledger := buildAgentLedger(active)
	if len(ledger.Completed) != 1 || ledger.Completed[0].Name != "get_goal" {
		t.Fatalf("completed tool evidence was not recovered: %#v", ledger)
	}
}

func TestActiveMessagesStartsAtRealNewUserTurn(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: "first task"},
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_goal", "arguments": "{}"}}}},
		{Role: "tool", ToolCallID: "call_1", Content: `{"goal":null}`},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second task"},
	}
	active := activeMessages(messages)
	if len(active) != 1 || contentToString(active[0].Content) != "second task" {
		t.Fatalf("new user turn included old tool history: %#v", active)
	}
}

func TestAgentLedgerDetectsRepeatedCallAndRoundLimit(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs, oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "poll", "arguments": "{\"id\":1}"}}}}, oaiMsg{Role: "tool", ToolCallID: id, Content: "still pending"})
	}
	l := buildAgentLedger(msgs)
	if !l.RepeatedCall || l.ToolRounds != 4 {
		t.Fatalf("loop not detected: %+v", l)
	}
	if err := l.CanContinue(3); err == nil {
		t.Fatal("expected round limit")
	}
}

func TestActiveMessagesIgnoresOlderToolHistory(t *testing.T) {
	var msgs []oaiMsg
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("old%d", i)
		msgs = append(msgs,
			oaiMsg{Role: "assistant", ToolCalls: []map[string]any{{"id": id, "type": "function", "function": map[string]any{"name": "old", "arguments": "{}"}}}},
			oaiMsg{Role: "tool", ToolCallID: id, Content: "done"},
		)
	}
	msgs = append(msgs, oaiMsg{Role: "user", Content: "continue with a new model"})
	full := buildAgentLedger(msgs)
	active := buildAgentLedger(activeMessages(msgs))
	if full.ToolRounds < 20 {
		t.Fatalf("expected full history tools, got %d", full.ToolRounds)
	}
	if active.ToolRounds != 0 {
		t.Fatalf("new user turn should reset round limit scope, got %d", active.ToolRounds)
	}
	if err := active.CanContinue(16); err != nil {
		t.Fatalf("new user turn blocked by old history: %v", err)
	}
}

func TestCompletionGuardRejectsPendingAndUnsupportedSuccess(t *testing.T) {
	l := buildAgentLedger([]oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{
			{"id": "p1", "type": "function", "function": map[string]any{"name": "deploy", "arguments": "{}"}},
		}},
	})
	if completionEvidenceAllows("Deployment completed successfully", l) {
		t.Fatal("pending action allowed as complete")
	}
}

func TestCompletionGuardRejectsUnsupportedSuccess(t *testing.T) {
	if completionEvidenceAllows("Installed, started, and verified successfully", buildAgentLedger(nil)) {
		t.Fatal("unsupported success allowed")
	}
	if !completionEvidenceAllows("I cannot confirm completion because no tool results were returned.", buildAgentLedger(nil)) {
		t.Fatal("honest incomplete response rejected")
	}
}

func TestCurrentTurnCanRepeatToolCompletedInOlderTurn(t *testing.T) {
	msgs := []oaiMsg{
		{Role: "assistant", ToolCalls: []map[string]any{{"id": "old-call", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"notes.md"}`}}}},
		{Role: "tool", ToolCallID: "old-call", Content: "old contents"},
		{Role: "user", Content: "Read notes.md again because it may have changed."},
	}
	candidate := []detectedToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.md"}`)}}
	if got := filterCompletedCalls(candidate, buildAgentLedger(msgs)); len(got) != 0 {
		t.Fatalf("full-history ledger should demonstrate the old regression, got %#v", got)
	}
	if got := filterCompletedCalls(candidate, buildAgentLedger(activeMessages(msgs))); len(got) != 1 {
		t.Fatalf("current turn incorrectly filtered a legitimate repeat: %#v", got)
	}
}

func TestShouldEnforceCompletionEvidenceRequiresActiveToolActivity(t *testing.T) {
	tools := []map[string]any{{"type": "function"}}
	if shouldEnforceCompletionEvidence(tools, agentLedger{}) {
		t.Fatal("tool schemas alone must not enable completion evidence enforcement")
	}
	if !shouldEnforceCompletionEvidence(tools, agentLedger{Completed: []toolEvidence{{ID: "call-1"}}}) {
		t.Fatal("completed active tool call must enable evidence enforcement")
	}
	if !shouldEnforceCompletionEvidence(tools, agentLedger{Pending: []toolEvidence{{ID: "call-2"}}}) {
		t.Fatal("pending active tool call must enable evidence enforcement")
	}
	if shouldEnforceCompletionEvidence(nil, agentLedger{Completed: []toolEvidence{{ID: "call-3"}}}) {
		t.Fatal("requests without tool schemas must not enable evidence enforcement")
	}
}
