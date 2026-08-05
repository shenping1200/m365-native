package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"m365-native/internal/chathub"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
	body   bytes.Buffer
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
	p.body.Write(b)
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string) {
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	go func() { s.openaiChat(irw, r2); _ = pw.Close() }()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	emit := func(name string, v any) {
		writeSSE(w, name, v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	type tcState struct {
		ID, Name, Args string
		ItemID         string
	}
	calls := map[int]*tcState{}
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) != nil {
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
				emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}})
			}
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": content})
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, _ := raw.(map[string]any)
				idx := int(tc["index"].(float64))
				st := calls[idx]
				if st == nil {
					st = &tcState{ItemID: "fc_" + uuid.NewString()}
					calls[idx] = st
					emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx, "item": map[string]any{"type": "function_call", "id": st.ItemID, "call_id": "", "name": "", "arguments": "", "status": "in_progress"}})
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
					emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": st.ItemID, "delta": v})
				}
			}
		}
	}
	if irw.status >= 400 {
		// The internal openaiChat call failed (auth, tool protocol, model
		// routing, ...). Without this the stream would close silently with only
		// response.created, and clients would retry a phantom failure while the
		// real error is lost. Emit response.failed so the error is observable.
		emit("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": id, "object": "response", "status": "failed", "model": model, "output": []any{}}, "error": map[string]any{"message": errorMessage(irw.body.Bytes(), "upstream protocol error"), "type": "upstream_error"}})
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" {
		// The upstream connection can close normally without producing a
		// response. Do not emit a completed Responses resource with an empty
		// message ID that clients may try to reference on the next turn.
		emit("response.failed", map[string]any{"type": "response.failed", "response": map[string]any{"id": id, "object": "response", "status": "failed", "model": model, "output": []any{}}, "error": map[string]any{"message": "upstream returned an empty response", "type": "upstream_error"}})
		return
	}
	output := []any{}
	if len(calls) > 0 {
		for i := 0; i < len(calls); i++ {
			st := calls[i]
			if st == nil {
				continue
			}
			item := map[string]any{"type": "function_call", "id": "fc_" + uuid.NewString(), "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": "completed"}
			output = append(output, item)
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i, "item": map[string]any{"type": "function_call", "id": item["id"], "call_id": st.ID, "name": st.Name, "arguments": "", "status": "in_progress"}})
			emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": item["id"], "delta": st.Args})
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": item["id"], "arguments": st.Args})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	} else {
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}
		output = append(output, item)
		if !textStarted {
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": text.String()})
		}
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": messageID, "text": text.String()})
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
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
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
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
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
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
	return strings.TrimSpace(contentText(msg["content"])) != ""
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	// Cap the client's max_tokens at the configured gateway limit. Copilot
	// does not honor max_tokens, but the protocol contract must not advertise
	// budgets the gateway cannot enforce.
	if maxOut := s.settings.get().MaxOutputTokens; maxOut > 0 && body.MaxTokens > maxOut {
		body.MaxTokens = maxOut
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if body.Stream {
		s.streamAnthropic(w, r, o, firstNonEmpty(body.Model, "m365-copilot"))
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	writeAnthropicResult(w, firstNonEmpty(body.Model, "m365-copilot"), false, out)
}

// streamAnthropic implements true Anthropic SSE streaming: upstream text and
// tool events are forwarded as they arrive instead of buffering the full
// completion and replaying it. This is what Claude Code and other Anthropic
// clients expect for responsive, long-running turns.
func (s *Server) streamAnthropic(w http.ResponseWriter, r *http.Request, o oaiReq, model string) {
	effort := o.ReasoningEffort
	if o.Reasoning != nil && strings.TrimSpace(o.Reasoning.Effort) != "" {
		effort = o.Reasoning.Effort
	}
	tone, toneErr := reasoningTone(o.Model, effort)
	if toneErr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", toneErr.Error())
		return
	}
	normalizeLegacyTools(&o)
	if err := validateToolConversation(o.Messages); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "tool_protocol_error", err.Error())
		return
	}
	ledger := buildAgentLedger(o.Messages)
	activeLedger := buildAgentLedger(activeMessages(o.Messages))
	if err := activeLedger.CanContinue(maxToolRounds()); err != nil {
		writeAnthropicError(w, http.StatusConflict, "tool_round_limit", err.Error())
		return
	}
	var prompt string
	prompt, o.Attachments = flattenPromptMessages(o.Messages, o.Attachments)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" && len(o.Attachments) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages required")
		return
	}
	poolKey := ""
	if o.ConversationID == "" && o.SessionID == "" {
		if anchor := conversationAnchor(o.Messages); anchor != "" {
			if keyID := s.apiKeys.keyID(requestAPIKey(r)); keyID != "" {
				poolKey = conversationPoolKey(keyID, o.Model, anchor)
				if pc, ok := s.convPool.get(poolKey); ok {
					o.AccountID = firstNonEmpty(o.AccountID, pc.AccountID)
					o.ConversationID = pc.ConversationID
					o.SessionID = pc.SessionID
				}
			}
		}
	}
	accountID := firstNonEmpty(o.AccountID, o.User)
	acc, err := s.resolveAccount(accountID)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if acc.OID == "" || acc.TID == "" {
		if a, t := extractOIDTID(acc.AccessToken); a != "" {
			acc.OID, acc.TID = a, t
		}
	}
	if acc.OID == "" || acc.TID == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "account missing oid/tid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.settings.get().ChatTimeoutSeconds)*time.Second)
	defer cancel()
	account := chathub.Account{AccessToken: acc.AccessToken, OID: acc.OID, TID: acc.TID, Proxy: acc.Proxy}

	answerReq := chathub.Request{
		Text:           prompt + "\n" + ledger.RouterContext() + "\nFINAL ANSWER RULE: Answer the user directly. If a tool is explicitly required, emit its structured call; otherwise return ordinary text.",
		Tone:           tone,
		ConversationID: o.ConversationID,
		SessionID:      o.SessionID,
		Attachments:    o.Attachments,
		Tools:          o.Tools,
		ToolChoice:     o.ToolChoice,
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	emit := func(name string, v any) {
		writeSSE(w, name, v)
		flusher.Flush()
	}
	id := "msg_" + uuid.NewString()
	emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "stop_reason": nil, "usage": anthropicUsage(prompt, "")}})

	blockIndex := 0
	textStarted := false
	toolStarted := false
	var outText strings.Builder
	var toolBlocks []map[string]any
	emitTextBlock := func() {
		if textStarted {
			return
		}
		textStarted = true
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "text", "text": ""}})
	}
	emitToolBlock := func(name string, id string) {
		textStarted = false // Anthropic alternates blocks; text stops when a tool starts
		toolStarted = true
		toolBlocks = append(toolBlocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
		emit("content_block_start", map[string]any{"type": "content_block_start", "index": blockIndex, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
	}
	stopToolBlock := func() {
		if !toolStarted {
			return
		}
		toolStarted = false
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}
	stopTextBlock := func() {
		if !textStarted {
			return
		}
		textStarted = false
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIndex})
		blockIndex++
	}

	res, err := s.chat.ChatWithEvents(ctx, account, answerReq, func(ev chathub.StreamEvent) error {
		if ev.Kind == "tool" && ev.ToolName != "" && len(ev.Arguments) > 0 {
			outText.Write(ev.Arguments)
			stopTextBlock()
			if !toolStarted {
				emitToolBlock(ev.ToolName, "toolu_"+uuid.NewString()[:16])
			}
			emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(ev.Arguments)}})
			return nil
		}
		if ev.Kind != "text" || ev.Text == "" {
			return nil
		}
		outText.WriteString(ev.Text)
		stopToolBlock()
		emitTextBlock()
		emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": blockIndex, "delta": map[string]any{"type": "text_delta", "text": ev.Text}})
		return nil
	})
	stopTextBlock()
	stopToolBlock()
	stopReason := "end_turn"
	if len(toolBlocks) > 0 {
		stopReason = "tool_use"
	}
	emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]any{"output_tokens": estimateTokens(outText.String())}})
	emit("message_stop", map[string]any{"type": "message_stop"})
	if poolKey != "" && res.ConversationID != "" {
		s.convPool.record(poolKey, pooledConversation{AccountID: acc.ID, ConversationID: res.ConversationID, SessionID: res.SessionID})
	}
	_ = err
}
