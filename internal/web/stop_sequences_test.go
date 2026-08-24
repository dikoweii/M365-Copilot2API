package web

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestStopSequencesAcceptStringAndArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{name: "string", raw: `"END"`, want: []string{"END"}},
		{name: "array", raw: `["END","DONE"]`, want: []string{"END", "DONE"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got stopSequences
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("stop sequences = %#v", got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("stop sequences = %#v", got)
				}
			}
		})
	}
}

func TestStopSequenceStreamFilterDoesNotLeakCrossChunkPrefix(t *testing.T) {
	filter := newStopSequenceStreamFilter(stopSequences{"<STOP>"})
	var got string
	got += filter.Push("answer<ST")
	got += filter.Push("OP>hidden")
	got += filter.Flush()
	if got != "answer" {
		t.Fatalf("filtered stream = %q", got)
	}
	if !filter.Stopped() || filter.Matched() != "<STOP>" {
		t.Fatalf("stop state = stopped:%t matched:%q", filter.Stopped(), filter.Matched())
	}
}

func TestStopSequenceStreamFilterFlushesSafeSuffix(t *testing.T) {
	filter := newStopSequenceStreamFilter(stopSequences{"<STOP>"})
	got := filter.Push("answer<ST") + filter.Flush()
	if got != "answer<ST" {
		t.Fatalf("flushed stream = %q", got)
	}
}

func TestApplyStopToChatCompletionSkipsToolCalls(t *testing.T) {
	withTool := map[string]any{"choices": []any{map[string]any{"message": map[string]any{
		"content":    "answer<STOP>hidden",
		"tool_calls": []any{map[string]any{"id": "call_1"}},
	}}}}
	if matched := applyStopToChatCompletion(withTool, stopSequences{"<STOP>"}); matched != "" {
		t.Fatalf("tool response matched stop %q", matched)
	}
	msg, _ := openAIChoice(withTool)
	if msg["content"] != "answer<STOP>hidden" {
		t.Fatalf("tool response content was truncated: %q", msg["content"])
	}
}

func TestAnthropicStopSequenceResponse(t *testing.T) {
	src := map[string]any{"choices": []any{map[string]any{
		"message":       map[string]any{"content": "answer<STOP>hidden"},
		"finish_reason": "stop",
	}}}
	matched := applyStopToChatCompletion(src, stopSequences{"<STOP>"})
	rr := httptest.NewRecorder()
	writeAnthropicResult(rr, "claude-sonnet", false, src, matched)
	var got struct {
		StopReason   string `json:"stop_reason"`
		StopSequence string `json:"stop_sequence"`
		Content      []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "stop_sequence" || got.StopSequence != "<STOP>" {
		t.Fatalf("stop response = reason:%q sequence:%q", got.StopReason, got.StopSequence)
	}
	if len(got.Content) != 1 || got.Content[0].Text != "answer" {
		t.Fatalf("content = %#v", got.Content)
	}
}
