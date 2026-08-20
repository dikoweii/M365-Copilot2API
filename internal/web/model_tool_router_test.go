package web

import (
	"strings"
	"testing"
)

func TestParseModelToolDecisionAutoAndParallel(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[{"name":"get_weather","arguments":{"city":"Beijing"}},{"name":"get_time","arguments":{"city":"Beijing"}}]}`, testTools(), "auto")
	if !ok || len(calls) != 2 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestParseModelToolDecisionNoCall(t *testing.T) {
	calls, ok := parseModelToolDecision(`{"calls":[]}`, testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
func TestModelToolRouterPromptMarksCompletedResults(t *testing.T) {
	p := modelToolRouterPrompt(`assistant tool_calls: [...]
tool[call_x]: 2026-07-18`, testTools(), "auto")
	if !strings.Contains(p, "Completed evidence must not be repeated") || !strings.Contains(p, "tool[call_x]: 2026-07-18") || !strings.Contains(p, "unfinished work remains") {
		t.Fatalf("missing multi-turn evidence constraint: %s", p)
	}
}

func TestModelToolRouterPromptAllowsFinalAnswerAfterCompletedTool(t *testing.T) {
	p := modelToolRouterPromptForTurn("completed evidence", testTools(), "auto", true)
	if !strings.Contains(p, "FINAL_ANSWER:") || !strings.Contains(p, "must not claim unverified actions") || strings.Contains(p, "NO_TOOL_NEEDED") {
		t.Fatalf("missing routed final-answer rules: %s", p)
	}
}

func TestParseModelToolRouteDecisionFinalAnswer(t *testing.T) {
	calls, answer, ok := parseModelToolRouteDecision("final_answer: DSH_M365_TOOL_OK", testTools(), "auto")
	if !ok || len(calls) != 0 || answer != "DSH_M365_TOOL_OK" {
		t.Fatalf("calls=%v answer=%q ok=%v", calls, answer, ok)
	}
}

func TestParseModelToolRouteDecisionKeepsToolCalls(t *testing.T) {
	calls, answer, ok := parseModelToolRouteDecision(`CALL_TOOL: get_weather({"city":"Beijing"})`, testTools(), "auto")
	if !ok || len(calls) != 1 || answer != "" {
		t.Fatalf("calls=%v answer=%q ok=%v", calls, answer, ok)
	}
}

func TestParseModelToolDecisionRejectsBadSchema(t *testing.T) {
	calls, ok := parseModelToolDecision("```json\n{\"calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":2}}]}\n```", testTools(), "auto")
	if !ok || len(calls) != 0 {
		t.Fatalf("calls=%v ok=%v", calls, ok)
	}
}
