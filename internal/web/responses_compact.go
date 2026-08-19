package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"

	"github.com/google/uuid"
)

const (
	compactionCapsulePrefix = "m365c1."
	maxCompactionSummary    = 1 << 20
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
	o.ConversationID = ""
	o.ConversationIDC = ""
	o.SessionID = ""
	o.SessionIDC = ""
	o.SessionKey = ""
	o.Messages = append([]oaiMsg{{Role: "system", Content: compactionInstructions}}, o.Messages...)
	o.Messages = append(o.Messages, oaiMsg{Role: "user", Content: "Write the compact continuation state now. Return only the continuation state."})

	out, raw, status, err := s.runOpenAIAdapter(withTransientModelRequest(r), o)
	if status >= http.StatusBadRequest {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream compaction failed"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream compaction protocol error: "+err.Error())
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
	estimate := estimateResponsesUsage(model, inputMessages, nil, "none", summary)
	trace.markFirstToken()
	s.recordUsage(r, auth.AccountToken{}, UsageRecord{
		Time:         time.Now(),
		Model:        model,
		Endpoint:     "/v1/responses/compact",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       http.StatusOK,
		UsageSource:  estimate.Source,
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
