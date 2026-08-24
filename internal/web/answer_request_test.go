package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"m365-copilot2api/internal/chathub"
)

func answerRequestTestBody() oaiReq {
	return oaiReq{
		ConversationID: "conversation-1",
		SessionID:      "session-1",
		Tools: []chathub.Tool{{
			Type:     "function",
			Function: json.RawMessage(`{"name":"read_file","parameters":{"type":"object"}}`),
		}},
		ToolChoice: "auto",
	}
}

func TestBuildAnswerRequestRouterOmitsNativePlugins(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router", "")
	if len(req.Tools) != 0 || req.ToolChoice != nil {
		t.Fatalf("router answer leaked native tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if !strings.HasPrefix(req.Text, "[user]\nhello\n") || !strings.Contains(req.Text, callerWorkspaceBoundaryRule) {
		t.Fatalf("router answer lost caller workspace boundary: %q", req.Text)
	}
}

func TestBuildAnswerRequestNativeForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "native", "")
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("native answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if !strings.Contains(req.Text, callerWorkspaceBoundaryRule) {
		t.Fatalf("native answer lost caller workspace boundary: %q", req.Text)
	}
}

func TestBuildAnswerRequestPreservesCallerWindowsPath(t *testing.T) {
	path := `E:\san\dsguomo\projects\guo-mo-chang\prose\drafts\chapter-010.md`
	req := buildAnswerRequest("[user]\n请读取 "+path, "magic", answerRequestTestBody(), agentLedger{}, "router", "")
	if !strings.Contains(req.Text, path) {
		t.Fatalf("caller path was changed: %q", req.Text)
	}
	if !strings.Contains(req.Text, "/mnt/data") || !strings.Contains(req.Text, "Do not infer") {
		t.Fatalf("answer prompt lacks provider-filesystem prohibition: %q", req.Text)
	}
}

func TestBuildAnswerRequestAddsCompletedEvidence(t *testing.T) {
	ledger := agentLedger{Completed: []toolEvidence{{ID: "call_1", Name: "read_file", Arguments: `{}`, Result: "ok"}}}
	req := buildAnswerRequest("[user]\nsummarize", "magic", answerRequestTestBody(), ledger, "router", "")
	for _, want := range []string{"EVIDENCE_LEDGER:", "Report only actions supported by completed tool results"} {
		if !strings.Contains(req.Text, want) {
			t.Fatalf("answer prompt missing %q: %s", want, req.Text)
		}
	}
}

func TestBuildAnswerRequestMCPForwardsTools(t *testing.T) {
	req := buildAnswerRequest("[user]\nhello", "magic", answerRequestTestBody(), agentLedger{}, "router", "http://127.0.0.1:4142/v1/mcp/sse")
	if len(req.Tools) != 1 || req.ToolChoice != "auto" {
		t.Fatalf("MCP answer lost tools: tools=%d choice=%#v", len(req.Tools), req.ToolChoice)
	}
	if req.MCPServerURL != "http://127.0.0.1:4142/v1/mcp/sse" {
		t.Fatalf("MCP URL not set: %q", req.MCPServerURL)
	}
}

func TestBuildAnswerRequestToolChoiceNoneOmitsTools(t *testing.T) {
	body := answerRequestTestBody()
	body.ToolChoice = "none"
	req := buildAnswerRequest("[user]\nhello", "magic", body, agentLedger{}, "native", "http://127.0.0.1:4142/v1/mcp/sse")
	if len(req.Tools) != 0 || req.ToolChoice != nil || req.MCPServerURL != "" {
		t.Fatalf("tool_choice=none forwarded tools: tools=%d choice=%#v mcp=%q", len(req.Tools), req.ToolChoice, req.MCPServerURL)
	}
	if strings.Contains(req.Text, callerWorkspaceBoundaryRule) {
		t.Fatalf("tool_choice=none should not add caller workspace rules: %q", req.Text)
	}
}

func TestNormalizeRequestToolsChoiceNoneClearsModernAndLegacyTools(t *testing.T) {
	body := answerRequestTestBody()
	body.Functions = []json.RawMessage{json.RawMessage(`{"name":"legacy_read","parameters":{"type":"object"}}`)}
	body.FunctionCall = "none"
	body.ToolChoice = nil

	normalizeRequestTools(&body)
	if len(body.Tools) != 0 || len(body.Functions) != 0 {
		t.Fatalf("tool_choice=none retained tools: modern=%d legacy=%d", len(body.Tools), len(body.Functions))
	}
	if !toolChoiceDisablesTools(body.ToolChoice) {
		t.Fatalf("legacy function_call=none was not normalized: %#v", body.ToolChoice)
	}
}

func TestApplyRequestSessionKeyUsesHeaderWithoutOverridingBody(t *testing.T) {
	for _, header := range requestSessionHeaderNames {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			req.Header.Set(header, "header-session")
			body := oaiReq{}
			applyRequestSessionKey(&body, req)
			if body.SessionKey != "header-session" {
				t.Fatalf("header session key = %q", body.SessionKey)
			}
		})
	}

	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-M365-Session-Id", "header-session")
	body := oaiReq{}
	body.SessionKey = "body-session"
	applyRequestSessionKey(&body, req)
	if body.SessionKey != "body-session" {
		t.Fatalf("body session key was overwritten: %q", body.SessionKey)
	}
}

func TestRequestSessionKeyUsesStableHeaderPriority(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Session-Id", "legacy")
	req.Header.Set("X-Claude-Code-Session-Id", "claude")
	req.Header.Set("X-Session-Id", "generic")
	req.Header.Set("X-M365-Session-Id", "m365")
	if got := requestSessionKey(req); got != "m365" {
		t.Fatalf("request session key = %q, want m365", got)
	}
}

func TestConfiguredMCPServerURLUsesTrustedHTTPSBase(t *testing.T) {
	t.Setenv("M365_MCP_PUBLIC_URL", "https://m365.example.test/gateway")
	t.Setenv("M365_PUBLIC_URL", "https://fallback.example.test")
	got, err := configuredMCPServerURL("tools-a")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://m365.example.test/gateway/v1/mcp/sse?scope=tools-a" {
		t.Fatalf("MCP URL = %q", got)
	}
}

func TestConfiguredMCPServerURLRejectsUntrustedShapes(t *testing.T) {
	for _, raw := range []string{"http://m365.example.test", "https://user@example.test", "https://m365.example.test?host=attacker"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("M365_MCP_PUBLIC_URL", raw)
			t.Setenv("M365_PUBLIC_URL", "")
			if got, err := configuredMCPServerURL("tools-a"); err == nil {
				t.Fatalf("accepted %q as %q", raw, got)
			}
		})
	}
}
