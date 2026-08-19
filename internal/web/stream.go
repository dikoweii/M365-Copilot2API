package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"m365-copilot2api/internal/auth"
	"m365-copilot2api/internal/chathub"
)

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	r, trace := ensureUsageTrace(r, startedAt)
	w, finishUsage := s.observeUsageResponse(w, r, startedAt, func() (auth.AccountToken, UsageRecord) {
		return auth.AccountToken{}, UsageRecord{Model: "m365-copilot", Endpoint: "/api/chat/stream", Stream: true}
	})
	defer finishUsage()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body chatBody
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(firstNonEmpty(body.Message, body.Prompt))
	if text == "" {
		http.Error(w, "message required", http.StatusBadRequest)
		return
	}
	if body.SessionKey != "" {
		if v, ok := s.sessions.get(body.SessionKey); ok {
			body.AccountID = firstNonEmpty(body.AccountID, v.AccountID)
			body.ConversationID = firstNonEmpty(body.ConversationID, v.ConversationID)
			body.SessionID = firstNonEmpty(body.SessionID, v.SessionID)
		}
	}
	acc, err := s.resolveAccount(body.AccountID)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if o, t := extractOIDTID(acc.AccessToken); o != "" {
			acc.OID, acc.TID = o, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		http.Error(w, "account missing oid/tid", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	requestID := requestIDFrom(r)
	if err := writeSSE(r, w, flusher, "connected", map[string]any{"type": "connected", "requestId": requestID}); err != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	identityFilter := newPublicIdentityStreamFilter(defaultPublicModelName)
	reasoningFilter := newPublicReasoningStreamFilter()
	delivery := &streamDeliveryState{}
	deltaIndex := 0
	toolCallCount := 0
	emitDelta := func(kind, value string) error {
		if value == "" {
			return nil
		}
		payload := map[string]any{"index": deltaIndex, "type": "chathub." + kind, "requestId": requestID}
		if kind == "reasoning" {
			payload["reasoning"] = value
		} else {
			payload["delta"] = value
		}
		if err := writeVisibleSSE(r, w, flusher, kind, payload, delivery, trace); err != nil {
			return err
		}
		deltaIndex++
		return nil
	}
	onEvent := func(event chathub.StreamEvent) error {
		switch event.Kind {
		case "text":
			return emitDelta("delta", identityFilter.Push(event.Text))
		case "reasoning":
			return emitDelta("reasoning", reasoningFilter.Push(event.Text))
		default:
			if event.Kind == "tool" {
				toolCallCount++
				trace.setToolCalls(toolCallCount)
			}
			return writeVisibleSSE(r, w, flusher, "progress", map[string]any{
				"type": event.Kind, "text": event.Text, "tool": event.ToolName,
				"arguments": event.Arguments, "requestId": requestID,
			}, delivery, trace)
		}
	}
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID}
	request := chathub.Request{Text: text, Tone: body.Tone, ConversationID: body.ConversationID, SessionID: body.SessionID, Attachments: body.Attachments}
	res, err := s.chatWithAccountEvents(ctx, acc.ID, account, request, onEvent)
	if err != nil && delivery.canRetry() && body.AccountID == "" && (IsRateLimited(err) || IsAuthFailure(err)) {
		if next, nextErr := s.nextHealthyAccount(acc.ID); nextErr == nil {
			request.ConversationID = ""
			request.SessionID = ""
			identityFilter = newPublicIdentityStreamFilter(defaultPublicModelName)
			reasoningFilter = newPublicReasoningStreamFilter()
			deltaIndex = 0
			toolCallCount = 0
			res, err = s.chatWithAccountEvents(withRetryAttempt(ctx), next.ID, chathub.Account{AccessToken: next.AccessToken, OID: next.OID, TID: next.TID}, request, onEvent)
			if err == nil {
				acc = next
			}
		}
	}
	if err != nil {
		message := sanitizePublicInternalText(upstreamError(err))
		status := upstreamStatus(err)
		failureType := usageErrorType(status)
		if res.Incomplete {
			failureType = "stream_interrupted"
		}
		trace.fail(status, failureType, message)
		s.recordUsage(r, acc, UsageRecord{
			Model: "m365-copilot", Endpoint: "/api/chat/stream", Stream: true,
			InputTokens: EstimateTokens(text), DurationMs: time.Since(startedAt).Milliseconds(),
			Status: status, ErrorType: usageErrorType(status), ErrorMessage: message,
		})
		payload := map[string]any{"type": "error", "message": message, "code": streamErrorCode(err), "requestId": requestID}
		if res.Incomplete {
			payload["code"] = "stream_interrupted"
			payload["partial"] = true
			payload["upstreamRequestId"] = res.RequestID
			payload["failureStage"] = res.FailureStage
			payload["lastDeltaMs"] = res.LastDeltaMs
			if res.UpstreamCloseCode != 0 {
				payload["upstreamCloseCode"] = res.UpstreamCloseCode
			}
		}
		_ = writeSSE(r, w, flusher, "error", payload)
		return
	}
	if value := identityFilter.Flush(); value != "" {
		if err := emitDelta("delta", value); err != nil {
			return
		}
	}
	if value := reasoningFilter.Flush(); value != "" {
		if err := emitDelta("reasoning", value); err != nil {
			return
		}
	}
	if body.SessionKey != "" {
		s.sessions.upsert(conversation{ID: body.SessionKey, AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID, Title: text})
	}
	res.Text = sanitizePublicAssistantText(res.Text)
	res.Reasoning = sanitizePublicReasoningText(res.Reasoning)
	for i, event := range res.Normalized {
		if !emitCompatibilityEvent(event) {
			continue
		}
		payload := map[string]any{
			"index":          i,
			"type":           "chathub.event",
			"event":          event,
			"conversationId": res.ConversationID,
			"sessionId":      res.SessionID,
			"requestId":      res.RequestID,
		}
		if err := writeSSE(r, w, flusher, "event", payload); err != nil {
			return
		}
	}
	for i, event := range chathub.SemanticEvents(res.Events) {
		if !emitCompatibilitySemantic(event) {
			continue
		}
		if err := writeSSE(r, w, flusher, "semantic", map[string]any{"index": i, "type": "m365.semantic", "event": event}); err != nil {
			return
		}
	}
	if err := writeSSE(r, w, flusher, "done", map[string]any{
		"type": "done", "text": res.Text,
		"conversationId": res.ConversationID, "sessionId": res.SessionID, "requestId": res.RequestID,
		"throttling": res.Throttling,
	}); err != nil {
		return
	}
	s.recordUsage(r, acc, UsageRecord{
		Model: "m365-copilot", Endpoint: "/api/chat/stream", Stream: true,
		InputTokens: EstimateTokens(text), OutputTokens: EstimateTokens(res.Text),
		DurationMs: time.Since(startedAt).Milliseconds(), Status: http.StatusOK,
	})
}

func emitCompatibilityEvent(event chathub.Event) bool {
	return event.Kind != "update"
}

func emitCompatibilitySemantic(event chathub.SemanticEvent) bool {
	return event.Kind != "message" || event.ContentType != "" || event.MessageType != ""
}

func writeVisibleSSE(r *http.Request, w http.ResponseWriter, flusher http.Flusher, name string, value any, delivery *streamDeliveryState, trace *usageTrace) error {
	if err := writeSSE(r, w, flusher, name, value); err != nil {
		return err
	}
	delivery.markVisible(trace)
	return nil
}

// writeSSE emits one SSE frame, returning when the client has disconnected
// (request context canceled) or the write fails so the handler can abort
// instead of blocking a goroutine against a dead socket.
func writeSSE(r *http.Request, w http.ResponseWriter, f http.Flusher, name string, value any) error {
	if err := r.Context().Err(); err != nil {
		return err
	}
	b, _ := json.Marshal(value)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, b); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}
