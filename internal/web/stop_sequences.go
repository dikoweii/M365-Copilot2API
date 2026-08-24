package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

type stopSequences []string

func (s *stopSequences) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*s = normalizeStopSequences([]string{single})
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("stop must be a string or an array of strings")
	}
	*s = normalizeStopSequences(many)
	return nil
}

func normalizeStopSequences(sequences []string) stopSequences {
	out := make(stopSequences, 0, len(sequences))
	seen := make(map[string]struct{}, len(sequences))
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if _, ok := seen[sequence]; ok {
			continue
		}
		seen[sequence] = struct{}{}
		out = append(out, sequence)
	}
	return out
}

func earliestStop(text string, sequences stopSequences) (int, string) {
	index := -1
	matched := ""
	for _, sequence := range sequences {
		if current := strings.Index(text, sequence); current >= 0 && (index < 0 || current < index || current == index && len(sequence) < len(matched)) {
			index = current
			matched = sequence
		}
	}
	return index, matched
}

func truncateTextAtStop(text string, sequences stopSequences) (string, string) {
	index, matched := earliestStop(text, sequences)
	if index < 0 {
		return text, ""
	}
	return text[:index], matched
}

type stopSequenceStreamFilter struct {
	sequences stopSequences
	pending   string
	matched   string
	stopped   bool
}

func newStopSequenceStreamFilter(sequences stopSequences) *stopSequenceStreamFilter {
	return &stopSequenceStreamFilter{sequences: normalizeStopSequences(sequences)}
}

func (f *stopSequenceStreamFilter) Push(part string) string {
	if f == nil || f.stopped || part == "" {
		return ""
	}
	if len(f.sequences) == 0 {
		return part
	}
	f.pending += part
	if index, matched := earliestStop(f.pending, f.sequences); index >= 0 {
		out := f.pending[:index]
		f.pending = ""
		f.matched = matched
		f.stopped = true
		return out
	}

	keep := 0
	for _, sequence := range f.sequences {
		limit := len(sequence) - 1
		if limit > len(f.pending) {
			limit = len(f.pending)
		}
		for size := limit; size > keep; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				keep = size
				break
			}
		}
	}
	out := f.pending[:len(f.pending)-keep]
	f.pending = f.pending[len(f.pending)-keep:]
	return out
}

func (f *stopSequenceStreamFilter) Flush() string {
	if f == nil || f.stopped {
		return ""
	}
	out := f.pending
	f.pending = ""
	return out
}

func (f *stopSequenceStreamFilter) Stopped() bool {
	return f != nil && f.stopped
}

func (f *stopSequenceStreamFilter) Matched() string {
	if f == nil {
		return ""
	}
	return f.matched
}

func applyStopToChatCompletion(src map[string]any, sequences stopSequences) string {
	if len(sequences) == 0 {
		return ""
	}
	msg, _ := openAIChoice(src)
	if msg == nil {
		return ""
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return ""
	}
	content, ok := msg["content"].(string)
	if !ok {
		return ""
	}
	truncated, matched := truncateTextAtStop(content, sequences)
	if matched != "" {
		msg["content"] = truncated
	}
	return matched
}
