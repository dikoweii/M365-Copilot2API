package web

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestBoundedToolRouterPromptContextKeepsHeadTailAndUTF8(t *testing.T) {
	source := "SYSTEM_CONSTRAINT_KEEP\n" + strings.Repeat("中间历史", 45000) + "\n请读取 E:\\san\\dsguomo\\projects\\guo-mo-chang\\prose\\drafts\\chapter-010.md，只返回第一行。"
	bounded, truncated := boundedToolRouterPromptContext(source)
	if !truncated {
		t.Fatal("expected oversized router context to be truncated")
	}
	if len(bounded) > toolRouterContextMaxBytes {
		t.Fatalf("bounded context length = %d, max = %d", len(bounded), toolRouterContextMaxBytes)
	}
	if !utf8.ValidString(bounded) {
		t.Fatal("bounded context is not valid UTF-8")
	}
	for _, want := range []string{"SYSTEM_CONSTRAINT_KEEP", toolRouterOmissionMarker, "chapter-010.md", "只返回第一行"} {
		if !strings.Contains(bounded, want) {
			t.Fatalf("bounded context lost %q", want)
		}
	}
}

func TestModelToolRouterPromptExplainsCallerWorkspaceBoundary(t *testing.T) {
	prompt := modelToolRouterPrompt("read E:\\work\\chapter.md", workspaceTestTools(), "auto")
	if !strings.Contains(prompt, "Caller tools operate in the caller workspace") || !strings.Contains(prompt, "/mnt/data") {
		t.Fatalf("missing caller workspace rule: %s", prompt)
	}
}

func TestShouldRetryMissingActionToolUsesRecentPathContext(t *testing.T) {
	messages := []oaiMsg{
		{Role: "user", Content: `请处理 E:\san\dsguomo\projects\guo-mo-chang\prose\drafts\chapter-010.md`},
		{Role: "assistant", Content: "我会检查。"},
		{Role: "user", Content: "改一下"},
	}
	if !shouldRetryMissingActionTool(messages, workspaceTestTools(), "auto", true, nil, "") {
		t.Fatal("expected a missing caller tool retry for a local edit request")
	}
	if shouldRetryMissingActionTool([]oaiMsg{{Role: "user", Content: "介绍一下文件系统原理"}}, workspaceTestTools(), "auto", true, nil, "") {
		t.Fatal("ordinary conceptual question must not force a caller tool")
	}
	if shouldRetryMissingActionTool(messages, testTools(), "auto", true, nil, "") {
		t.Fatal("non-workspace tools must not trigger local action recovery")
	}
	if shouldRetryMissingActionTool(messages, workspaceTestTools(), "required", true, nil, "") {
		t.Fatal("required mode already has its own constrained retry")
	}
}

func TestMissingActionToolRetryPromptIsRequiredAndRecent(t *testing.T) {
	messages := []oaiMsg{
		{Role: "system", Content: strings.Repeat("old-system-context", 4000)},
		{Role: "user", Content: `请读取 E:\san\dsguomo\projects\guo-mo-chang\prose\drafts\chapter-010.md`},
	}
	prompt := missingActionToolRetryPrompt(messages, workspaceTestTools())
	for _, want := range []string{"MODE: required", "At least one tool call is required", "chapter-010.md", "Do not answer the task"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("retry prompt missing %q", want)
		}
	}
}

func TestShouldRetryFailedFileToolOverridesPrematureFinalAnswer(t *testing.T) {
	messages := []oaiMsg{{Role: "user", Content: `请读取 E:\san\dsguomo\projects\guo-mo-chang\prose\drafts\chapter-010.md`}}
	ledger := agentLedger{Completed: []toolEvidence{{
		ID:        "call_failed",
		Name:      "read",
		Arguments: `{"path":"E:\\san\\dsguomo\\projects\\guo-mo-chang\\prose\\drafts\\chapter-010.md"}`,
		Result:    "failed: transient workspace read error",
		Failed:    true,
	}}}
	if !shouldRetryFailedFileTool(messages, workspaceTestTools(), "auto", ledger, true, nil) {
		t.Fatal("failed caller file operation should force another tool selection")
	}
	if shouldRetryFailedFileTool(messages, workspaceTestTools(), "none", ledger, true, nil) {
		t.Fatal("tool_choice=none must not retry a failed file tool")
	}
	ledger.Completed[0].Failed = false
	if shouldRetryFailedFileTool(messages, workspaceTestTools(), "auto", ledger, true, nil) {
		t.Fatal("successful file evidence must not force another tool")
	}
}

func TestFailedFileToolRetryPromptKeepsCallerPathAndFailureEvidence(t *testing.T) {
	path := `E:\san\dsguomo\projects\guo-mo-chang\prose\drafts\chapter-010.md`
	ledger := agentLedger{Completed: []toolEvidence{{
		ID:        "call_failed",
		Name:      "read",
		Arguments: `{"path":"E:\\san\\dsguomo\\projects\\guo-mo-chang\\prose\\drafts\\chapter-010.md"}`,
		Result:    "permission denied",
		Failed:    true,
	}}}
	prompt := failedFileToolRetryPrompt([]oaiMsg{{Role: "user", Content: "请读取 " + path}}, workspaceTestTools(), ledger)
	for _, want := range []string{"MODE: required", path, "permission denied", "never repeat the same failed name and arguments unchanged", "do not rewrite the caller path", "/mnt/data"} {
		if !strings.Contains(strings.ToLower(prompt), strings.ToLower(want)) {
			t.Fatalf("failed-file retry prompt missing %q: %s", want, prompt)
		}
	}
}

func workspaceTestTools() []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "Read a file from the caller workspace",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"},
			},
		},
	}}
}
