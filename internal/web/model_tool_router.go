package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	toolRouterContextMaxBytes        = 96 * 1024
	toolRouterContextHeadBytes       = 16 * 1024
	missingActionToolContextMaxBytes = 24 * 1024
)

const toolRouterOmissionMarker = "\n\n[... older conversation omitted for tool routing ...]\n\n"

const callerWorkspaceBoundaryRule = `CALLER WORKSPACE RULE: Caller tools operate in the caller workspace. Do not infer /mnt/data, a cloud sandbox, or any provider filesystem as the caller workspace. Use a declared caller tool before claiming that a local path is unavailable or that a local action succeeded.`

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	return modelToolRouterPromptForTurn(prompt, tools, choice, false)
}

func modelToolRouterPromptForTurn(prompt string, tools []map[string]any, choice any, allowFinalAnswer bool) string {
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	noToolRule := "- If no tool is needed, respond with: NO_TOOL_NEEDED"
	if strings.EqualFold(mode, "required") {
		noToolRule = "- At least one tool call is required; do not respond with NO_TOOL_NEEDED"
	}
	if allowFinalAnswer {
		noToolRule = `- Completed tool evidence is available. If no additional tool is needed, respond with: FINAL_ANSWER: <user-facing answer>
- The final answer must rely only on completed evidence and must not claim unverified actions`
	}
	rules := `- If a tool is needed, respond with: CALL_TOOL: tool_name({"arg1":"value1"})
%s
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list`
	rules = fmt.Sprintf(rules, noToolRule)
	// Multi-turn: completed tool evidence (tool[...], tool_calls:) was already
	// acted upon, so re-invoking those tools would duplicate work.
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") {
		rules += `
- Completed evidence must not be repeated: tool_calls/tool[call_x] rows are prior results already delivered to the user, never re-invoke them
- Only start a new tool call when fresh unfinished work remains on the current request`
	}
	if hasCallerWorkspaceTool(tools) {
		rules += "\n- " + callerWorkspaceBoundaryRule
	}
	return fmt.Sprintf(`You are a tool selection assistant. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and evidence:
%s`, defs, mode, rules, prompt)
}

func boundedToolRouterPromptContext(prompt string) (string, bool) {
	if !utf8.ValidString(prompt) {
		prompt = strings.ToValidUTF8(prompt, "")
	}
	if len(prompt) <= toolRouterContextMaxBytes {
		return prompt, false
	}
	tailBytes := toolRouterContextMaxBytes - toolRouterContextHeadBytes - len(toolRouterOmissionMarker)
	head := utf8Prefix(prompt, toolRouterContextHeadBytes)
	tail := utf8Suffix(prompt, tailBytes)
	return head + toolRouterOmissionMarker + tail, true
}

func utf8Prefix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(text[:end]) {
		end--
	}
	return text[:end]
}

func utf8Suffix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.ValidString(text[start:]) {
		start++
	}
	return text[start:]
}

func hasCallerWorkspaceTool(tools []map[string]any) bool {
	keywords := []string{
		`"name":"read"`, `"name":"write"`, `"name":"edit"`,
		"read_file", "write_file", "edit_file", "apply_patch", "exec_command",
		"filesystem", "workspace", "terminal", "shell", "powershell", "bash",
		"view_image", "git status", "file path", "directory path",
	}
	for _, tool := range tools {
		encoded, _ := json.Marshal(tool)
		definition := strings.ToLower(string(encoded))
		for _, keyword := range keywords {
			if strings.Contains(definition, keyword) {
				return true
			}
		}
	}
	return false
}

func shouldRetryMissingActionTool(messages []oaiMsg, tools []map[string]any, choice any, parsed bool, calls []detectedToolCall, routedAnswer string) bool {
	mode := normalizedToolChoiceMode(choice)
	if !parsed || len(calls) > 0 || strings.TrimSpace(routedAnswer) != "" || strings.EqualFold(mode, "none") || strings.EqualFold(mode, "required") {
		return false
	}
	if !hasCallerWorkspaceTool(tools) {
		return false
	}
	request := latestUserToolRequest(messages)
	if !containsToolActionIntent(request) {
		return false
	}
	recent := recentToolRoutingContext(messages, missingActionToolContextMaxBytes)
	return containsCallerResourceHint(request) || containsCallerResourceHint(recent) || explicitlyRequestsTool(request)
}

func shouldRetryFailedFileTool(messages []oaiMsg, tools []map[string]any, choice any, ledger agentLedger, parsed bool, calls []detectedToolCall) bool {
	mode := normalizedToolChoiceMode(choice)
	if !parsed || len(calls) > 0 || strings.EqualFold(mode, "none") || strings.EqualFold(mode, "required") {
		return false
	}
	if !hasFailedCallerFileEvidence(ledger, tools) {
		return false
	}
	request := latestUserToolRequest(messages)
	if !containsToolActionIntent(request) {
		return false
	}
	recent := recentToolRoutingContext(messages, missingActionToolContextMaxBytes)
	return containsCallerResourceHint(request) || containsCallerResourceHint(recent) || explicitlyRequestsTool(request)
}

func hasFailedCallerFileEvidence(ledger agentLedger, tools []map[string]any) bool {
	for _, evidence := range ledger.Completed {
		if evidence.Failed && isCallerFileTool(evidence.Name, tools) {
			return true
		}
	}
	return false
}

func isCallerFileTool(name string, tools []map[string]any) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, tool := range tools {
		fn, _ := tool["function"].(map[string]any)
		toolName := strings.ToLower(strings.TrimSpace(fmt.Sprint(fn["name"])))
		if toolName != name {
			continue
		}
		encoded, _ := json.Marshal(tool)
		definition := strings.ToLower(string(encoded))
		nameHints := []string{"read", "write", "edit", "patch", "file", "glob", "grep", "search", "list_dir", "directory"}
		definitionHints := []string{"file", "path", "directory", "filesystem", "workspace"}
		for _, hint := range nameHints {
			if strings.Contains(toolName, hint) {
				return true
			}
		}
		for _, hint := range definitionHints {
			if strings.Contains(definition, hint) {
				return true
			}
		}
	}
	return false
}

func latestUserToolRequest(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			return strings.TrimSpace(contentToString(messages[i].Content))
		}
	}
	return ""
}

func containsToolActionIntent(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		"读取", "打开", "查看", "检查", "审计", "修改", "更改", "改一下", "改下", "改写", "编辑", "写入", "保存", "创建", "删除", "运行", "执行", "构建", "测试", "部署", "提交", "搜索", "查找", "列出", "下载", "上传", "继续",
		" read ", " open ", " inspect ", " check ", " audit ", " modify ", " edit ", " rewrite ", " write ", " save ", " create ", " delete ", " remove ", " run ", " execute ", " build ", " test ", " deploy ", " commit ", " search ", " find ", " list ", " download ", " upload ", " continue ", " proceed ",
	}
	padded := " " + lower + " "
	for _, keyword := range keywords {
		if strings.Contains(padded, keyword) {
			return true
		}
	}
	return false
}

func containsCallerResourceHint(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{
		":\\", ":/", "\\\\", "/home/", "/users/", "/workspace/", "/project/",
		"文件", "目录", "路径", "工作区", "项目", "仓库", "代码", "脚本", "命令", "终端", "日志", "截图", "数据库",
		" file", " directory", " path", " workspace", " project", " repository", " repo", " source", " script", " command", " terminal", " log", " database",
		".md", ".txt", ".json", ".yaml", ".yml", ".go", ".js", ".ts", ".tsx", ".py", ".ps1", ".sh",
	}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func explicitlyRequestsTool(text string) bool {
	lower := strings.ToLower(text)
	keywords := []string{"调用工具", "使用工具", "实际调用", "call a tool", "use a tool", "tool call"}
	for _, keyword := range keywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func recentToolRoutingContext(messages []oaiMsg, maxBytes int) string {
	start := len(messages) - 8
	if start < 0 {
		start = 0
	}
	var b strings.Builder
	for _, message := range messages[start:] {
		content := strings.TrimSpace(contentToString(message.Content))
		if content == "" {
			continue
		}
		b.WriteString(strings.ToUpper(strings.TrimSpace(message.Role)))
		b.WriteString(":\n")
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	context := b.String()
	if len(context) <= maxBytes {
		return context
	}
	return "[... earlier recent context omitted ...]\n" + utf8Suffix(context, maxBytes-len("[... earlier recent context omitted ...]\n"))
}

func missingActionToolRetryPrompt(messages []oaiMsg, tools []map[string]any) string {
	recent := recentToolRoutingContext(messages, missingActionToolContextMaxBytes)
	evidence := `The first routing pass selected no tool, but the current user turn requests an operation on caller-controlled resources.
Choose the single best next caller tool. Do not answer the task and do not claim that a path is unavailable without calling a declared tool.

Recent caller context:
` + recent
	return modelToolRouterPromptForTurn(evidence, tools, "required", false)
}

func failedFileToolRetryPrompt(messages []oaiMsg, tools []map[string]any, ledger agentLedger) string {
	recent := recentToolRoutingContext(messages, missingActionToolContextMaxBytes)
	evidence := `A declared caller file tool failed and the requested local operation is still unfinished.
Select the single best next declared caller tool. Use a different tool or materially different arguments; never repeat the same failed name and arguments unchanged.
Do not answer the task, do not rewrite the caller path, and do not substitute /mnt/data or a provider sandbox.

Recent caller context:
` + recent + "\n" + ledger.RouterContext()
	return modelToolRouterPromptForTurn(evidence, tools, "required", false)
}

func parseModelToolRouteDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, string, bool) {
	text = strings.TrimSpace(text)
	const finalPrefix = "FINAL_ANSWER:"
	if len(text) >= len(finalPrefix) && strings.EqualFold(text[:len(finalPrefix)], finalPrefix) {
		answer := strings.TrimSpace(text[len(finalPrefix):])
		return nil, answer, answer != ""
	}
	calls, parsed := parseModelToolDecision(text, tools, choice)
	return calls, "", parsed
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	// Try the new natural language format first: CALL_TOOL: name({...})
	if strings.HasPrefix(text, "CALL_TOOL:") || strings.HasPrefix(text, "call_tool:") {
		parts := strings.SplitN(text, ":", 2)
		if len(parts) == 2 {
			rest := strings.TrimSpace(parts[1])
			start := strings.Index(rest, "(")
			end := strings.LastIndex(rest, ")")
			if start > 0 && end > start {
				name := strings.TrimSpace(rest[:start])
				argsStr := rest[start+1 : end]
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
					fn := toolFunction(name, tools)
					if fn != nil && schemaValid(args, fn) == nil {
						b, _ := json.Marshal(args)
						return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
					}
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text[start:end+1]), &probe) != nil {
		return nil, false
	}
	if _, ok := probe["calls"]; !ok {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	for i, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}
