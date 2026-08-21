package web

import (
	"fmt"
	"strings"
)

type outputTokenLimiter struct {
	max       int
	count     func(string) int
	emitted   strings.Builder
	truncated bool
}

func newOutputTokenLimiter(model string, maxTokens int) *outputTokenLimiter {
	count, _ := tokenEstimator(model)
	return &outputTokenLimiter{max: maxTokens, count: count}
}

func (l *outputTokenLimiter) Apply(text string) string {
	if text == "" || l == nil || l.max <= 0 {
		return text
	}
	if l.truncated {
		return ""
	}
	kept, truncated := tokenLimitedPrefix(l.emitted.String(), text, l.max, l.count)
	l.emitted.WriteString(kept)
	if truncated {
		l.truncated = true
	}
	return kept
}

func (l *outputTokenLimiter) Truncated() bool {
	return l != nil && l.truncated
}

func (l *outputTokenLimiter) Tokens() int {
	if l == nil || l.count == nil {
		return 0
	}
	return l.count(l.emitted.String())
}

func limitAssistantText(model, reasoning, content string, maxTokens int) (string, string, bool, int) {
	limiter := newOutputTokenLimiter(model, maxTokens)
	reasoning = limiter.Apply(reasoning)
	content = limiter.Apply(content)
	return reasoning, content, limiter.Truncated(), limiter.Tokens()
}

// applyAnthropicOutputLimit trims the already-adapted completion before it is
// rendered as either JSON or SSE. Text is sliced on rune boundaries and sized
// with the same local estimator used by protocol usage accounting.
func applyAnthropicOutputLimit(src map[string]any, model string, maxTokens int) bool {
	if maxTokens <= 0 {
		return false
	}
	choices, _ := src["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return false
	}
	sanitizePublicAssistantMessage(msg, model)

	count, _ := tokenEstimator(model)
	emitted := ""
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		kept, truncated := tokenLimitedPrefix(emitted, reasoning, maxTokens, count)
		msg["reasoning_content"] = kept
		emitted += kept
		if truncated {
			msg["content"] = ""
			delete(msg, "tool_calls")
			markAnthropicMaxTokens(src, choice, count(emitted))
			return true
		}
	}

	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		kept := make([]any, 0, len(calls))
		for _, raw := range calls {
			segment := anthropicToolCallLimitText(raw)
			if count(emitted+segment) > maxTokens {
				if len(kept) == 0 {
					delete(msg, "tool_calls")
					msg["content"] = ""
				} else {
					msg["tool_calls"] = kept
				}
				markAnthropicMaxTokens(src, choice, count(emitted))
				return true
			}
			kept = append(kept, raw)
			emitted += segment
		}
		return false
	}

	switch content := msg["content"].(type) {
	case string:
		kept, truncated := tokenLimitedPrefix(emitted, content, maxTokens, count)
		msg["content"] = kept
		emitted += kept
		if truncated {
			markAnthropicMaxTokens(src, choice, count(emitted))
			return true
		}
	case []any:
		keptParts := make([]any, 0, len(content))
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok || part["type"] != "text" {
				keptParts = append(keptParts, raw)
				continue
			}
			text, _ := part["text"].(string)
			kept, truncated := tokenLimitedPrefix(emitted, text, maxTokens, count)
			part["text"] = kept
			keptParts = append(keptParts, part)
			emitted += kept
			if truncated {
				msg["content"] = keptParts
				markAnthropicMaxTokens(src, choice, count(emitted))
				return true
			}
		}
	}
	return false
}

func tokenLimitedPrefix(base, text string, maxTokens int, count func(string) int) (string, bool) {
	if text == "" || count(base+text) <= maxTokens {
		return text, false
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := low + (high-low+1)/2
		if count(base+string(runes[:mid])) <= maxTokens {
			low = mid
		} else {
			high = mid - 1
		}
	}
	for low > 0 && count(base+string(runes[:low])) > maxTokens {
		low--
	}
	return string(runes[:low]), true
}

func anthropicToolCallLimitText(raw any) string {
	call, _ := raw.(map[string]any)
	fn, _ := call["function"].(map[string]any)
	return fmt.Sprint(fn["name"]) + fmt.Sprint(fn["arguments"])
}

func markAnthropicMaxTokens(src, choice map[string]any, outputTokens int) {
	choice["finish_reason"] = "length"
	usage, _ := src["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
		src["usage"] = usage
	}
	usage["completion_tokens"] = int64(outputTokens)
	if promptTokens, ok := numericTokenValue(usage["prompt_tokens"]); ok {
		usage["total_tokens"] = promptTokens + int64(outputTokens)
	}
}

func numericTokenValue(value any) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case float32:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
