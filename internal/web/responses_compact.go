package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"m365-copilot2api/internal/auth"

	"github.com/google/uuid"
)

const (
	compactionCapsulePrefix  = "m365c1."
	maxCompactionSummary     = 1 << 20
	maxCompactionInputTokens = int64(48_000)
)

const compactionInstructions = `Create a compact continuation state for another coding agent. Do not answer the user's task and do not call tools. Preserve the user's current goal, binding instructions, decisions, exact paths and identifiers, completed changes, verification evidence, failures, pending work, and important constraints. Remove repetition and obsolete discussion. Never invent completed work. Return plain text only, preferably under 12000 characters.`

type transientModelRequestKey struct{}

func withTransientModelRequest(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), transientModelRequestKey{}, true))
}

func isTransientModelRequest(r *http.Request) bool {
	v, _ := r.Context().Value(transientModelRequestKey{}).(bool)
	return v
}

func isolateTransientModelRequest(body *oaiReq) {
	body.ConversationID = ""
	body.ConversationIDC = ""
	body.SessionID = ""
	body.SessionIDC = ""
	body.SessionKey = ""
	body.User = ""
}

// normalizeCompactionMessages converts tool protocol records into plain
// transcript entries. Compaction summarizes history and never executes tools,
// so an interrupted historical call must not make the summary request fail the
// strict live-turn tool validator.
func normalizeCompactionMessages(messages []oaiMsg) []oaiMsg {
	out := cloneMessages(messages)
	for i := range out {
		m := &out[i]
		if len(m.ToolCalls) > 0 {
			parts := make([]string, 0, 2)
			if content := strings.TrimSpace(contentToString(m.Content)); content != "" {
				parts = append(parts, content)
			}
			parts = append(parts, "Historical tool calls (results may be missing):\n"+mustJSON(m.ToolCalls))
			m.Content = strings.Join(parts, "\n\n")
			m.ToolCalls = nil
			m.ToolCallID = ""
			continue
		}
		if strings.EqualFold(strings.TrimSpace(m.Role), "tool") {
			id := strings.TrimSpace(m.ToolCallID)
			if id == "" {
				id = "unknown"
			}
			m.Role = "system"
			m.Content = fmt.Sprintf("Historical tool result (call_id=%s):\n%s", id, contentToString(m.Content))
			m.ToolCallID = ""
			m.ToolCalls = nil
		}
	}
	return out
}

func boundedCompactionTranscript(messages []oaiMsg, maxTokens int64) (transcript string, sourceTokens int64, truncated bool) {
	transcript, _ = flattenPromptMessages(normalizeCompactionMessages(messages), nil)
	sourceTokens = EstimateTokens(transcript)
	if maxTokens <= 0 || sourceTokens <= maxTokens {
		return transcript, sourceTokens, false
	}

	const marker = "\n\n[Earlier middle history omitted by the gateway to fit the upstream compaction budget.]\n\n"
	maxBytes := int(maxTokens * 4)
	contentBytes := maxBytes - len(marker)
	if contentBytes < 16 {
		return marker, sourceTokens, true
	}
	headBytes := contentBytes / 4
	tailBytes := contentBytes - headBytes
	headEnd := minUTF8Boundary(transcript, headBytes)
	tailStart := maxUTF8Boundary(transcript, len(transcript)-tailBytes)
	if headEnd >= tailStart {
		return transcript, sourceTokens, false
	}
	return transcript[:headEnd] + marker + transcript[tailStart:], sourceTokens, true
}

func minUTF8Boundary(text string, end int) int {
	if end >= len(text) {
		return len(text)
	}
	if end <= 0 {
		return 0
	}
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}

func maxUTF8Boundary(text string, start int) int {
	if start <= 0 {
		return 0
	}
	if start >= len(text) {
		return len(text)
	}
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return start
}

func encodeCompactionSummary(summary string) (string, error) {
	var compressed bytes.Buffer
	zw, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if err != nil {
		return "", err
	}
	if _, err := io.WriteString(zw, summary); err != nil {
		_ = zw.Close()
		return "", err
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	return compactionCapsulePrefix + base64.RawURLEncoding.EncodeToString(compressed.Bytes()), nil
}

func decodeCompactionSummary(capsule string) (string, error) {
	if !strings.HasPrefix(capsule, compactionCapsulePrefix) {
		return "", fmt.Errorf("unsupported encrypted_content")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(capsule, compactionCapsulePrefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted_content: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("open encrypted_content: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(zr, maxCompactionSummary+1))
	closeErr := zr.Close()
	if err != nil {
		return "", fmt.Errorf("read encrypted_content: %w", err)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close encrypted_content: %w", closeErr)
	}
	if len(b) > maxCompactionSummary {
		return "", fmt.Errorf("compaction summary exceeds %d bytes", maxCompactionSummary)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("compaction summary is empty")
	}
	return string(b), nil
}

func (s *Server) responsesCompact(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	r, trace := ensureUsageTrace(r, startedAt)
	var body responsesRequest
	w, finishUsage := s.observeUsageResponse(w, r, startedAt, func() (auth.AccountToken, UsageRecord) {
		return auth.AccountToken{}, UsageRecord{Model: firstNonEmpty(body.Model, "m365-copilot"), Endpoint: "/v1/responses/compact"}
	})
	defer finishUsage()
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "bad json")
		return
	}
	body.Stream = false
	body.Tools = nil
	body.ToolChoice = "none"
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	inputMessages := cloneMessages(o.Messages)
	o.Stream = false
	o.Tools = nil
	o.Functions = nil
	o.ToolChoice = "none"
	isolateTransientModelRequest(&o)
	transcript, sourceTokens, transcriptTruncated := boundedCompactionTranscript(o.Messages, maxCompactionInputTokens)
	o.Messages = []oaiMsg{
		{Role: "system", Content: compactionInstructions},
		{Role: "system", Content: "Conversation transcript to compact:\n" + transcript},
		{Role: "user", Content: "Write the compact continuation state now. Return only the continuation state."},
	}
	if transcriptTruncated {
		log.Printf("[responses-compact] bounded oversized transcript source_tokens=%d budget_tokens=%d", sourceTokens, maxCompactionInputTokens)
	}

	out, raw, status, err := s.runOpenAIAdapter(withTransientModelRequest(r), o)
	if status >= http.StatusBadRequest {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream compaction failed"))
		return
	}
	if err != nil {
		log.Printf("[responses-compact] adapter failed: %v", err)
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream compaction protocol error")
		return
	}
	msg, _ := openAIChoice(out)
	summary := ""
	if msg != nil {
		summary, _ = msg["content"].(string)
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream compaction returned an empty summary")
		return
	}
	capsule, err := encodeCompactionSummary(summary)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "server_error", "failed to encode compacted context")
		return
	}

	model := firstNonEmpty(body.Model, "m365-copilot")
	estimate := estimateResponsesUsage(model, o.Messages, nil, "none", summary)
	originalEstimate := estimateResponsesUsage(model, inputMessages, nil, "none", "")
	omittedTokens := int64(originalEstimate.Values["input_tokens"].(int) - estimate.Values["input_tokens"].(int))
	if omittedTokens < 0 {
		omittedTokens = 0
	}
	trace.markFirstToken()
	s.recordUsage(r, auth.AccountToken{}, UsageRecord{
		Time:          time.Now(),
		Model:         model,
		Endpoint:      "/v1/responses/compact",
		InputTokens:   int64(estimate.Values["input_tokens"].(int)),
		OutputTokens:  int64(estimate.Values["output_tokens"].(int)),
		HistoryTokens: omittedTokens,
		DurationMs:    time.Since(startedAt).Milliseconds(),
		Status:        http.StatusOK,
		UsageSource:   estimate.Source,
	})
	jsonOut(w, map[string]any{
		"id":         "resp_" + uuid.NewString(),
		"object":     "response.compaction",
		"created_at": time.Now().Unix(),
		"output": []any{map[string]any{
			"type":              "compaction",
			"encrypted_content": capsule,
		}},
		"usage": estimate.Values,
		"m365":  localUsageMetadata(estimate.Source),
	})
}
