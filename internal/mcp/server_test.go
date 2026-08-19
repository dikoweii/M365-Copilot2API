package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestScopedToolRegistryIsolation(t *testing.T) {
	r := &toolRegistry{}
	r.RegisterToolsForScope("scope-a", []Tool{{Name: "read_file"}})
	r.RegisterToolsForScope("scope-b", []Tool{{Name: "run_command"}})

	a := r.ListToolsForScope("scope-a")
	b := r.ListToolsForScope("scope-b")
	if len(a) != 1 || a[0].Name != "read_file" {
		t.Fatalf("scope-a tools = %#v", a)
	}
	if len(b) != 1 || b[0].Name != "run_command" {
		t.Fatalf("scope-b tools = %#v", b)
	}
	if got := r.ListTools(); len(got) != 0 {
		t.Fatalf("scoped tools leaked into global registry: %#v", got)
	}

	r.ClearScope("scope-a")
	if got := r.ListToolsForScope("scope-a"); len(got) != 0 {
		t.Fatalf("cleared scope still has tools: %#v", got)
	}
	if got := r.ListToolsForScope("scope-b"); len(got) != 1 {
		t.Fatalf("clearing scope-a changed scope-b: %#v", got)
	}
}

func TestHandleToolsListUsesRequestedScope(t *testing.T) {
	GlobalToolRegistry.ClearTools()
	t.Cleanup(GlobalToolRegistry.ClearTools)
	GlobalToolRegistry.RegisterToolsForScope("request-1", []Tool{{Name: "workspace_read"}})

	req := httptest.NewRequest("GET", "/v1/mcp/tools?scope=request-1", nil)
	rr := httptest.NewRecorder()
	HandleToolsList(rr, req)

	var body struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 1 || body.Tools[0].Name != "workspace_read" {
		t.Fatalf("tools response = %#v", body.Tools)
	}
}

func TestScopedSessionToolsListNeverFallsBackToGlobalRegistry(t *testing.T) {
	GlobalToolRegistry.ClearTools()
	GlobalToolRegistry.RegisterTools([]Tool{{Name: "global_tool"}})
	t.Cleanup(GlobalToolRegistry.ClearTools)

	id := int64(1)
	request := &jsonRPCRequest{JSONRPC: "2.0", ID: &id, Method: "tools/list"}
	for _, tc := range []struct {
		name string
		tool string
	}{
		{name: "scope-a", tool: "workspace_read"},
		{name: "scope-b", tool: "run_command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &session{provider: NewStaticToolProvider([]Tool{{Name: tc.tool}}, nil)}
			response := handleRPC(context.Background(), sess, request)
			if response == nil || response.Error != nil {
				t.Fatalf("tools/list response = %#v", response)
			}
			var result struct {
				Tools []Tool `json:"tools"`
			}
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tools) != 1 || result.Tools[0].Name != tc.tool {
				t.Fatalf("scoped tools = %#v", result.Tools)
			}
		})
	}
}

func TestScopedProviderQueuesToolCalls(t *testing.T) {
	GlobalToolRegistry.ClearTools()
	t.Cleanup(GlobalToolRegistry.ClearTools)
	queue := NewToolCallQueue()
	tools := []Tool{{Name: "workspace_read"}}
	GlobalToolRegistry.RegisterProviderForScope("scope-call", tools, NewMCPToolProvider(tools, queue))

	provider := GlobalToolRegistry.ProviderForScope("scope-call")
	if provider == nil {
		t.Fatal("scoped provider missing")
	}
	result, err := provider.CallTool(context.Background(), "workspace_read", map[string]any{"path": "README.md"})
	if err != nil || len(result.Content) == 0 {
		t.Fatalf("call result=%#v err=%v", result, err)
	}
	queued := queue.DequeueNonBlocking()
	if queued == nil || queued.Name != "workspace_read" || queued.Arguments["path"] != "README.md" {
		t.Fatalf("queued call=%#v", queued)
	}
}

func TestScopedSSESessionsKeepJSONRPCToolsIsolated(t *testing.T) {
	GlobalToolRegistry.ClearTools()
	GlobalToolRegistry.RegisterToolsForScope("scope-a", []Tool{{Name: "workspace_read"}})
	GlobalToolRegistry.RegisterToolsForScope("scope-b", []Tool{{Name: "run_command"}})
	t.Cleanup(GlobalToolRegistry.ClearTools)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/mcp/sse", HandleSSE)
	mux.HandleFunc("/v1/mcp/message", HandleMessage)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	type connection struct {
		body     io.ReadCloser
		reader   *bufio.Reader
		endpoint string
		cancel   context.CancelFunc
	}
	readEvent := func(reader *bufio.Reader) (string, string) {
		t.Helper()
		event := ""
		data := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("read SSE event: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				return event, data
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			}
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}
	connect := func(scope string) connection {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/mcp/sse?scope="+url.QueryEscape(scope), nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			cancel()
			t.Fatalf("scope %s SSE status = %d", scope, resp.StatusCode)
		}
		reader := bufio.NewReader(resp.Body)
		event, endpoint := readEvent(reader)
		if event != "endpoint" || endpoint == "" {
			resp.Body.Close()
			cancel()
			t.Fatalf("scope %s endpoint event = %q %q", scope, event, endpoint)
		}
		return connection{body: resp.Body, reader: reader, endpoint: endpoint, cancel: cancel}
	}
	listTools := func(conn connection) []Tool {
		t.Helper()
		response, err := server.Client().Post(conn.endpoint, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		event, data := readEvent(conn.reader)
		if event != "message" {
			t.Fatalf("tools/list SSE event = %q, data=%q", event, data)
		}
		var rpcResponse jsonRPCResponse
		if err := json.Unmarshal([]byte(data), &rpcResponse); err != nil {
			t.Fatal(err)
		}
		if rpcResponse.Error != nil {
			t.Fatalf("tools/list RPC error = %#v", rpcResponse.Error)
		}
		var result struct {
			Tools []Tool `json:"tools"`
		}
		if err := json.Unmarshal(rpcResponse.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result.Tools
	}

	connA := connect("scope-a")
	defer connA.cancel()
	defer connA.body.Close()
	connB := connect("scope-b")
	defer connB.cancel()
	defer connB.body.Close()

	toolsA := listTools(connA)
	toolsB := listTools(connB)
	if len(toolsA) != 1 || toolsA[0].Name != "workspace_read" {
		t.Fatalf("scope-a JSON-RPC tools = %#v", toolsA)
	}
	if len(toolsB) != 1 || toolsB[0].Name != "run_command" {
		t.Fatalf("scope-b JSON-RPC tools = %#v", toolsB)
	}
}

func TestCapabilityRequestsRequireLiveScopeOrSession(t *testing.T) {
	GlobalToolRegistry.ClearTools()
	GlobalToolRegistry.RegisterToolsForScope("live-scope", []Tool{{Name: "workspace_read"}})
	t.Cleanup(GlobalToolRegistry.ClearTools)

	liveScope := httptest.NewRequest(http.MethodGet, "/v1/mcp/sse?scope=live-scope", nil)
	if !IsCapabilityRequest(liveScope) {
		t.Fatal("live scoped SSE request was not recognized as a capability")
	}
	unknownScope := httptest.NewRequest(http.MethodGet, "/v1/mcp/sse?scope=unknown", nil)
	if IsCapabilityRequest(unknownScope) {
		t.Fatal("unknown scoped SSE request was accepted")
	}
	unscoped := httptest.NewRequest(http.MethodGet, "/v1/mcp/sse", nil)
	if IsCapabilityRequest(unscoped) {
		t.Fatal("legacy unscoped SSE request bypassed API-key authentication")
	}

	sessionID := GlobalRegistry.RegisterSession(NewStaticToolProvider([]Tool{{Name: "workspace_read"}}, nil))
	t.Cleanup(func() { GlobalRegistry.UnregisterSession(sessionID) })
	liveSession := httptest.NewRequest(http.MethodPost, "/v1/mcp/message?sessionId="+url.QueryEscape(sessionID), nil)
	if !IsCapabilityRequest(liveSession) {
		t.Fatal("live MCP message session was not recognized as a capability")
	}
	unknownSession := httptest.NewRequest(http.MethodPost, "/v1/mcp/message?sessionId=unknown", nil)
	if IsCapabilityRequest(unknownSession) {
		t.Fatal("unknown MCP message session was accepted")
	}
}
