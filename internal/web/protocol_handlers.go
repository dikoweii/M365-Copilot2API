package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"m365-copilot2api/internal/auth"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
}

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string) {
	startedAt := time.Now()
	r, trace := ensureUsageTrace(r, startedAt)
	innerCtx, cancelInner := context.WithCancel(r.Context())
	defer cancelInner()
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(withInternalUsageAdapter(innerCtx))
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	innerErr := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("[responses] inner goroutine panic: %v", recovered)
				innerErr <- fmt.Errorf("inner chat stream panicked")
			}
			_ = pw.Close()
			close(innerDone)
		}()
		s.openaiChat(irw, r2)
	}()
	defer func() {
		cancelInner()
		_ = pr.CloseWithError(context.Canceled)
		select {
		case <-innerDone:
		case <-time.After(time.Second):
			log.Printf("[responses] inner adapter did not stop within cleanup timeout")
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	var writeMu sync.Mutex
	emit := func(name string, v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeSSE(r, w, flusher, name, v)
	}
	emitKeepalive := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return sseSafeRaw(w, flusher, ": keepalive\n\n")
	}
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	if err := emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}}); err != nil {
		return
	}
	keepaliveStop := make(chan struct{})
	keepaliveDone := make(chan struct{})
	go func() {
		defer close(keepaliveDone)
		ticker := time.NewTicker(sseKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveStop:
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				if err := emitKeepalive(); err != nil {
					cancelInner()
					return
				}
			}
		}
	}()
	defer func() {
		close(keepaliveStop)
		<-keepaliveDone
	}()

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	type tcState struct {
		ID, Name, Args, Type string
		ItemID               string
	}
	calls := map[int]*tcState{}
	var innerFailure map[string]any
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if r.Context().Err() != nil {
			trace.fail(499, "client_cancelled", "client disconnected before the response completed")
			return
		}
		line := scanner.Text()
		if line == ": keepalive" {
			if err := emitKeepalive(); err != nil {
				return
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
			continue
		}
		if failure, ok := chunk["error"].(map[string]any); ok {
			innerFailure = failure
			continue
		}
		if innerFailure != nil {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			if !textStarted {
				textStarted = true
				if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}}); err != nil {
					return
				}
			}
			if err := emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": content}); err != nil {
				return
			}
			trace.markFirstToken()
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, _ := raw.(map[string]any)
				idx := int(tc["index"].(float64))
				st := calls[idx]
				typ := "function"
				if v, ok := tc["type"].(string); ok && v == "custom" {
					typ = "custom"
				}
				if st == nil {
					prefix := "fc_"
					item := map[string]any{"type": "function_call", "call_id": "", "name": "", "arguments": "", "status": "in_progress"}
					if typ == "custom" {
						prefix = "ctc_"
						item = map[string]any{"type": "custom_tool_call", "call_id": "", "name": "", "input": "", "status": "in_progress"}
					}
					st = &tcState{ItemID: prefix + uuid.NewString(), Type: typ}
					calls[idx] = st
					item["id"] = st.ItemID
					if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item}); err != nil {
						return
					}
					trace.markFirstToken()
				}
				if v, ok := tc["id"].(string); ok {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					st.Name += v
				}
				if v, ok := fn["arguments"].(string); ok {
					st.Args += v
					if st.Type != "custom" {
						emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": st.ItemID, "delta": v})
					}
				}
			}
		}
	}
	<-innerDone
	select {
	case err := <-innerErr:
		if err != nil && innerFailure == nil {
			innerFailure = map[string]any{"code": "inner_stream_panic", "message": "inner chat stream failed"}
		}
	default:
	}
	if innerFailure != nil {
		code := firstNonEmpty(strings.TrimSpace(fmt.Sprint(innerFailure["code"])), "upstream_error")
		message := firstNonEmpty(strings.TrimSpace(fmt.Sprint(innerFailure["message"])), "inner chat stream failed")
		status := responsesStreamFailureStatus(innerFailure)
		trace.fail(status, code, message)
		errorBody := responsesStreamFailureBody(innerFailure)
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": errorBody,
			},
		})
		return
	}
	if scanner.Err() != nil || irw.status >= http.StatusBadRequest {
		status := irw.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		trace.fail(status, usageErrorType(status), "inner chat request failed")
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": status, "message": "inner chat request failed"},
			},
		})
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" {
		// Never leave a Responses stream after response.created without a
		// terminal event: clients otherwise render this as a successful blank
		// answer and may reuse an incomplete response on the next turn.
		trace.fail(http.StatusBadGateway, "empty_upstream_response", "ChatHub returned no text or tool call")
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": "empty_upstream_response", "message": "ChatHub returned no text or tool call"},
			},
		})
		return
	}
	output := []any{}
	if len(calls) > 0 {
		for i := 0; i < len(calls); i++ {
			st := calls[i]
			if st == nil {
				continue
			}
			if st.Type == "custom" {
				input := customToolInput(st.Args)
				item := map[string]any{"type": "custom_tool_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "input": input, "status": "completed"}
				output = append(output, item)
				emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": item["id"], "delta": input})
				emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": item["id"], "input": input})
				emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				continue
			}
			item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": "completed"}
			output = append(output, item)
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": st.ItemID, "arguments": st.Args})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	} else {
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}
		output = append(output, item)
		if !textStarted {
			if err := emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item}); err != nil {
				return
			}
			if err := emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": text.String()}); err != nil {
				return
			}
			trace.markFirstToken()
		}
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": messageID, "text": text.String()})
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	usageOutput := text.String()
	for _, call := range calls {
		usageOutput += call.Name + call.Args
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOutput)
	toolTokens := estimateToolTokens(model, o.Tools, o.ToolChoice)
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "usage": estimate.Values, "m365": localUsageMetadata(estimate.Source)}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
	s.recordUsage(r, auth.AccountToken{}, UsageRecord{
		Model: model, Endpoint: "/v1/responses", Stream: true,
		InputTokens:  int64(estimate.Values["input_tokens"].(int) - toolTokens),
		ToolTokens:   int64(toolTokens),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(), Status: http.StatusOK,
		UsageSource: estimate.Source,
	})
}

func responsesStreamFailureStatus(failure map[string]any) int {
	if value, ok := failure["status"].(float64); ok && value >= 400 && value <= 599 {
		return int(value)
	}
	code := strings.ToLower(strings.TrimSpace(fmt.Sprint(failure["code"])))
	switch {
	case strings.Contains(code, "rate_limit"), strings.Contains(code, "too_many_requests"):
		return http.StatusTooManyRequests
	case strings.Contains(code, "auth"), strings.Contains(code, "unauthorized"), strings.Contains(code, "forbidden"):
		return http.StatusUnauthorized
	case strings.Contains(code, "invalid_request"), strings.Contains(code, "tool_protocol"):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func responsesStreamFailureBody(failure map[string]any) map[string]any {
	body := make(map[string]any, len(failure)+2)
	for key, value := range failure {
		body[key] = value
	}
	body["code"] = firstNonEmpty(strings.TrimSpace(fmt.Sprint(failure["code"])), "upstream_error")
	body["message"] = firstNonEmpty(strings.TrimSpace(fmt.Sprint(failure["message"])), "inner chat stream failed")
	return body
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(withInternalUsageAdapter(r.Context()))
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, err
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	r, _ = ensureUsageTrace(r, startedAt)
	var body responsesRequest
	w, finishUsage := s.observeUsageResponse(w, r, startedAt, func() (auth.AccountToken, UsageRecord) {
		return auth.AccountToken{}, UsageRecord{Model: firstNonEmpty(body.Model, "m365-copilot"), Endpoint: "/v1/responses", Stream: body.Stream}
	})
	defer finishUsage()
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tenant := extractAPIKey(r)
	if body.PreviousResponseID != "" {
		s.responseMu.Lock()
		prior, ok := s.responseMessages[tenant][body.PreviousResponseID]
		messages := append([]oaiMsg(nil), prior.Messages...)
		s.responseMu.Unlock()
		if !ok || len(messages) == 0 {
			writeResponsesError(w, 400, "invalid_request_error", "unknown previous_response_id")
			return
		}
		o.Messages = append(messages, o.Messages...)
	}
	if body.Stream {
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(body.Model, "m365-copilot"))
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		log.Printf("[responses] adapter failed: %v", err)
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error")
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	msg, _ := openAIChoice(out)
	outputForUsage := ""
	if msg != nil {
		outputForUsage = fmt.Sprint(msg["content"])
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputForUsage += fmt.Sprint(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, outputForUsage)
	toolTokens := estimateToolTokens(firstNonEmpty(body.Model, "m365-copilot"), o.Tools, o.ToolChoice)
	out["usage"] = estimate.Values
	out["m365_usage_source"] = estimate.Source
	s.recordUsage(r, auth.AccountToken{}, UsageRecord{
		Time:         time.Now(),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/responses",
		InputTokens:  int64(estimate.Values["input_tokens"].(int) - toolTokens),
		ToolTokens:   int64(toolTokens),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
		UsageSource:  estimate.Source,
	})
	// Retain the normalized history so a subsequent previous_response_id can
	// validate its function_call_output against the original tool call.
	if _, ok := out["id"].(string); ok {
		// Use the same public response id that writeResponsesResult exposes.
		publicID := "resp_" + uuid.NewString()
		out["m365_response_id"] = publicID
		stored := append([]oaiMsg(nil), o.Messages...)
		if msg, _ := openAIChoice(out); msg != nil {
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				converted := make([]map[string]any, 0, len(calls))
				for _, call := range calls {
					if m, ok := call.(map[string]any); ok {
						converted = append(converted, m)
					}
				}
				stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
			} else {
				if text, _ := msg["content"].(string); text != "" {
					stored = append(stored, oaiMsg{Role: "assistant", Content: text})
				}
			}
		}
		s.responseMu.Lock()
		bucket := s.responseMessages[tenant]
		if bucket == nil {
			bucket = map[string]respHistory{}
			s.responseMessages[tenant] = bucket
		}
		for k, h := range bucket {
			if time.Since(h.At) > time.Hour {
				delete(bucket, k)
			}
		}
		if len(bucket) >= maxResponsesPerTenant {
			var oldestKey string
			var oldestAt time.Time
			for k, h := range bucket {
				if oldestKey == "" || h.At.Before(oldestAt) {
					oldestKey, oldestAt = k, h.At
				}
			}
			delete(bucket, oldestKey)
		}
		bucket[publicID] = respHistory{At: time.Now(), Messages: stored}
		s.responseMu.Unlock()
	}
	writeResponsesResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	text, _ := msg["content"].(string)
	return strings.TrimSpace(text) != ""
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	r, _ = ensureUsageTrace(r, startedAt)
	var body anthropicRequest
	w, finishUsage := s.observeUsageResponse(w, r, startedAt, func() (auth.AccountToken, UsageRecord) {
		return auth.AccountToken{}, UsageRecord{Model: firstNonEmpty(body.Model, "m365-copilot"), Endpoint: "/v1/messages", Stream: body.Stream}
	})
	defer finishUsage()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		log.Printf("[anthropic] adapter failed: %v", err)
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error")
		return
	}
	estimate := estimateResponsesUsage(firstNonEmpty(body.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, chatCompletionUsageOutput(out))
	toolTokens := estimateToolTokens(firstNonEmpty(body.Model, "m365-copilot"), o.Tools, o.ToolChoice)
	s.recordUsage(r, auth.AccountToken{}, UsageRecord{
		Time:         time.Now(),
		Model:        firstNonEmpty(body.Model, "m365-copilot"),
		Endpoint:     "/v1/messages",
		InputTokens:  int64(estimate.Values["input_tokens"].(int) - toolTokens),
		ToolTokens:   int64(toolTokens),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
		UsageSource:  estimate.Source,
	})
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), body.Stream, out)
}

func chatCompletionUsageOutput(src map[string]any) string {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return ""
	}
	var out strings.Builder
	out.WriteString(contentToString(msg["content"]))
	if reasoning, _ := msg["reasoning_content"].(string); reasoning != "" {
		out.WriteString(reasoning)
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			out.WriteString(fmt.Sprint(fn["name"]))
			out.WriteString(fmt.Sprint(fn["arguments"]))
		}
	}
	return out.String()
}
