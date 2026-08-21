package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestApplyAnthropicOutputLimitIsUTF8Safe(t *testing.T) {
	const original = "你好，世界。这里还有更多中文内容。"
	src := anthropicLimitTestSource(original)
	if !applyAnthropicOutputLimit(src, "gpt-5.5", 3) {
		t.Fatal("expected output to be truncated")
	}

	msg, finish := openAIChoice(src)
	text, _ := msg["content"].(string)
	if !utf8.ValidString(text) {
		t.Fatalf("truncated output is invalid UTF-8: %q", text)
	}
	if text == original || !strings.HasPrefix(original, text) {
		t.Fatalf("output was not a valid prefix: %q", text)
	}
	count, _ := tokenEstimator("gpt-5.5")
	if got := count(text); got > 3 {
		t.Fatalf("truncated output uses %d tokens, want <= 3", got)
	}
	if finish != "length" {
		t.Fatalf("finish_reason = %q", finish)
	}
	usage := src["usage"].(map[string]any)
	if got := usage["completion_tokens"].(int64); got > 3 {
		t.Fatalf("completion_tokens = %d", got)
	}
}

func TestAnthropicMaxTokensAppliesToJSONAndAdaptedStream(t *testing.T) {
	const original = "A long adapted response with several tokens that must be limited."

	plainSource := anthropicLimitTestSource(original)
	applyAnthropicOutputLimit(plainSource, "gpt-5.5", 4)
	plain := httptest.NewRecorder()
	writeAnthropicResult(plain, "gpt-5.5", false, plainSource)
	var body struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
		Usage      map[string]int64 `json:"usage"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid non-stream response: %v", err)
	}
	if body.StopReason != "max_tokens" {
		t.Fatalf("non-stream stop_reason = %q", body.StopReason)
	}
	if body.Usage["output_tokens"] > 4 {
		t.Fatalf("non-stream output_tokens = %d", body.Usage["output_tokens"])
	}
	plainText, _ := body.Content[0]["text"].(string)

	streamSource := anthropicLimitTestSource(original)
	applyAnthropicOutputLimit(streamSource, "gpt-5.5", 4)
	stream := httptest.NewRecorder()
	writeAnthropicResult(stream, "gpt-5.5", true, streamSource)
	streamBody := stream.Body.String()
	if !strings.Contains(streamBody, `"stop_reason":"max_tokens"`) {
		t.Fatalf("stream missing max_tokens stop reason: %s", streamBody)
	}
	if strings.Contains(streamBody, original) || !strings.Contains(streamBody, `"text":`+mustJSON(plainText)) {
		t.Fatalf("stream did not use the same truncated text: %s", streamBody)
	}
}

func TestAnthropicOutputLimitAppliesAfterPublicIdentitySanitization(t *testing.T) {
	src := anthropicLimitTestSource("I am Microsoft 365 Copilot. This answer continues after the identity statement.")
	if !applyAnthropicOutputLimit(src, "gpt-5.5", 4) {
		t.Fatal("expected sanitized output to be limited")
	}

	rr := httptest.NewRecorder()
	writeAnthropicResult(rr, "gpt-5.5", false, src)
	var body struct {
		Content    []map[string]any `json:"content"`
		StopReason string           `json:"stop_reason"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	text, _ := body.Content[0]["text"].(string)
	count, _ := tokenEstimator("gpt-5.5")
	if count(text) > 4 || body.StopReason != "max_tokens" {
		t.Fatalf("sanitized response exceeded limit: stop=%q text=%q", body.StopReason, text)
	}
	if strings.Contains(strings.ToLower(text), "microsoft 365") || strings.Contains(strings.ToLower(text), "copilot") {
		t.Fatalf("public identity was not sanitized: %q", text)
	}
}

func TestAnthropicOutputLimitDoesNotEmitPartialToolJSON(t *testing.T) {
	src := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":       "call_1",
					"function": map[string]any{"name": "weather", "arguments": `{"city":"Paris","units":"metric"}`},
				}},
			},
		}},
	}
	if !applyAnthropicOutputLimit(src, "gpt-5.5", 1) {
		t.Fatal("expected tool output to be limited")
	}
	msg, finish := openAIChoice(src)
	if _, ok := msg["tool_calls"]; ok {
		t.Fatalf("partial tool call was retained: %#v", msg)
	}
	if finish != "length" {
		t.Fatalf("finish_reason = %q", finish)
	}
}

func TestOutputTokenLimiterHandlesIncrementalUTF8(t *testing.T) {
	limiter := newOutputTokenLimiter("gpt-5.5", 4)
	var output strings.Builder
	for _, chunk := range []string{"你好，", "世界。", "这里不应完整出现。"} {
		output.WriteString(limiter.Apply(chunk))
	}
	text := output.String()
	if !utf8.ValidString(text) {
		t.Fatalf("incremental output is invalid UTF-8: %q", text)
	}
	count, _ := tokenEstimator("gpt-5.5")
	if got := count(text); got > 4 {
		t.Fatalf("incremental output uses %d tokens, want <= 4", got)
	}
	if !limiter.Truncated() {
		t.Fatal("expected incremental output to be truncated")
	}
}

func TestOpenAIOutputLimitPrecedenceAndResponsesMapping(t *testing.T) {
	chat := oaiReq{MaxTokens: 20, MaxCompletionTokens: 7}
	if got := chat.outputTokenLimit(); got != 7 {
		t.Fatalf("outputTokenLimit()=%d, want 7", got)
	}
	responses := responsesRequest{Input: "hello", MaxOutputTokens: 9}
	adapted, err := responses.openAI()
	if err != nil {
		t.Fatal(err)
	}
	if adapted.outputTokenLimit() != 9 {
		t.Fatalf("Responses max_output_tokens mapped to %d", adapted.outputTokenLimit())
	}
}

func TestResponsesProjectionMarksMaxOutputTokensIncomplete(t *testing.T) {
	source := anthropicLimitTestSource("limited output")
	choice := source["choices"].([]any)[0].(map[string]any)
	choice["finish_reason"] = "length"
	source["usage"] = map[string]any{"input_tokens": 7, "output_tokens": 8, "total_tokens": 15}
	applyResponsesOutputTokenLimit(source["usage"].(map[string]any), 5)

	plain := httptest.NewRecorder()
	writeResponsesResult(plain, "gpt-5.5", false, source)
	var body map[string]any
	if err := json.Unmarshal(plain.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "incomplete" {
		t.Fatalf("status=%v, want incomplete", body["status"])
	}
	details, _ := body["incomplete_details"].(map[string]any)
	if details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details=%v", details)
	}
	usage, _ := body["usage"].(map[string]any)
	if usage["output_tokens"] != float64(5) || usage["total_tokens"] != float64(12) {
		t.Fatalf("non-stream usage=%v, want output_tokens=5 total_tokens=12", usage)
	}

	stream := httptest.NewRecorder()
	writeResponsesResult(stream, "gpt-5.5", true, source)
	streamBody := stream.Body.String()
	if !strings.Contains(streamBody, "event: response.incomplete") || strings.Contains(streamBody, "event: response.completed") {
		t.Fatalf("unexpected terminal event: %s", streamBody)
	}
	if !strings.Contains(streamBody, `"output_tokens":5`) || !strings.Contains(streamBody, `"total_tokens":12`) {
		t.Fatalf("stream usage was not capped consistently: %s", streamBody)
	}
}

func TestResponsesUsageLimitLeavesUnlimitedUsageUnchanged(t *testing.T) {
	usage := map[string]any{"input_tokens": 7, "output_tokens": 8, "total_tokens": 15}
	applyResponsesOutputTokenLimit(usage, 0)
	if usage["output_tokens"] != 8 || usage["total_tokens"] != 15 {
		t.Fatalf("unlimited usage changed: %v", usage)
	}
}

func TestResponsesOutputAllowsEmptyLengthCompletion(t *testing.T) {
	source := anthropicLimitTestSource("")
	source["choices"].([]any)[0].(map[string]any)["finish_reason"] = "length"
	if !responsesOutputHasContent(source) {
		t.Fatal("length-limited empty response was rejected")
	}
}

func anthropicLimitTestSource(content string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{"prompt_tokens": float64(7), "completion_tokens": float64(100), "total_tokens": float64(107)},
	}
}
