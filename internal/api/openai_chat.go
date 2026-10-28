package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
)

var sseHeaders = map[string]string{
	"Cache-Control":     "no-cache, no-transform",
	"X-Accel-Buffering": "no",
	"Connection":        "keep-alive",
}

func writeSSEHeaders(w http.ResponseWriter) {
	for k, v := range sseHeaders {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
}

func sseData(value any) string {
	raw, _ := jsonMarshalNoEscape(value)
	return "data: " + raw + "\n\n"
}

func sseEvent(event string, value any) string {
	raw, _ := jsonMarshalNoEscape(value)
	return "event: " + event + "\ndata: " + raw + "\n\n"
}

func writeAndFlush(w http.ResponseWriter, payload string) error {
	if _, err := w.Write([]byte(payload)); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func chatChunk(id string, created int64, model string, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
}

func wantUsage(streamOptions any) bool {
	m, ok := streamOptions.(map[string]any)
	if !ok {
		return false
	}
	v, _ := m["include_usage"].(bool)
	return v
}

func paceText(text string, emit func(string) error) error {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	if len(runes) <= 3 {
		return emit(text)
	}
	const chunkSize = 2
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		if err := emit(string(runes[i:end])); err != nil {
			return err
		}
		if end < len(runes) {
			time.Sleep(12 * time.Millisecond)
		}
	}
	return nil
}

func (s *Server) prepareChatImages(w http.ResponseWriter, r *http.Request, messages []ChatMessage) ([]string, bool) {
	images := chatContentImages(messages)
	if len(images) == 0 {
		return nil, true
	}
	prepared, err := prepareReferences(r.Context(), s.eng, images)
	if err != nil {
		writeError(w, r, err)
		return nil, false
	}
	return prepared, true
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := decodeJSONBody(w, r, &req, 64<<20); err != nil {
		writeError(w, r, err)
		return
	}
	prompt := buildChatPrompt(req.Messages)
	if strings.TrimSpace(prompt) == "" {
		prompt = strings.TrimSpace(req.Prompt)
	}
	if strings.TrimSpace(strings.ReplaceAll(prompt, "[image]", "")) == "" {
		prompt = strings.TrimSpace(req.Prompt)
	}
	chatImages, ok := s.prepareChatImages(w, r, req.Messages)
	if !ok {
		return
	}
	if prompt == "" {
		if len(chatImages) == 0 {
			writeError(w, r, httpError(400, "messages are empty"))
			return
		}
		prompt = fmt.Sprintf("Respond to the %d attached image(s).", len(chatImages))
	} else if len(chatImages) > 0 {
		prompt = fmt.Sprintf("%s\n\n[%d image(s) are attached with this message.]", prompt, len(chatImages))
	}
	toolNote := ""
	if s.cfg.ToolProtocol {
		toolNote = buildToolsPrompt(req.Tools)
		if toolNote != "" {
			prompt = prompt + "\n\n" + toolNote
		}
	}
	model := resolveModel(req.Model, "muse-spark")
	timeout := s.cfg.ChatTimeout
	if req.Timeout != nil && *req.Timeout > 0 {
		timeout = *req.Timeout
	}
	timeoutDuration := time.Duration(timeout) * time.Second
	cid := "chatcmpl-" + randomHex(24)
	created := time.Now().Unix()

	if req.Stream {
		writeSSEHeaders(w)
		w.WriteHeader(200)
		if err := writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"role": "assistant"}, nil))); err != nil {
			return
		}
		var streamErr error
		if toolNote == "" {
			streamErr = s.StreamChat(r.Context(), prompt, chatImages, timeoutDuration, func(chunk string) error {
				return paceText(chunk, func(piece string) error {
					return writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"content": piece}, nil)))
				})
			})
			if streamErr == nil {
				_ = writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{}, "stop")))
			}
		} else {
			var buf strings.Builder
			streamErr = s.StreamChat(r.Context(), prompt, chatImages, timeoutDuration, func(chunk string) error {
				buf.WriteString(chunk)
				return nil
			})
			if streamErr == nil {
				calls, rest := parseToolCalls(buf.String())
				if len(calls) > 0 {
					if rest != "" {
						_ = paceText(rest, func(piece string) error {
							return writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"content": piece}, nil)))
						})
					}
					head := make([]any, 0, len(calls))
					body := make([]any, 0, len(calls))
					for i, c := range calls {
						fn := c["function"].(map[string]any)
						head = append(head, map[string]any{
							"index": i, "id": c["id"], "type": "function",
							"function": map[string]any{"name": fn["name"], "arguments": ""},
						})
						body = append(body, map[string]any{
							"index":    i,
							"function": map[string]any{"arguments": fn["arguments"]},
						})
					}
					_ = writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"tool_calls": head}, nil)))
					_ = writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"tool_calls": body}, nil)))
					_ = writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{}, "tool_calls")))
				} else {
					if buf.Len() > 0 {
						_ = paceText(buf.String(), func(piece string) error {
							return writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{"content": piece}, nil)))
						})
					}
					_ = writeAndFlush(w, sseData(chatChunk(cid, created, model, map[string]any{}, "stop")))
				}
			}
		}
		if streamErr != nil {
			if r.Context().Err() != nil {
				return
			}
			status := 500
			typ := "server_error"
			message := streamErr.Error()
			if engine.IsAuthError(streamErr) {
				status, typ = 401, "auth_error"
			} else if engine.IsRefusalError(streamErr) || !isContextError(streamErr) {
				status, typ = 502, "server_error"
			}
			_ = writeAndFlush(w, sseData(map[string]any{"error": map[string]any{
				"message": message, "type": typ, "code": status,
			}}))
			return
		}
		if wantUsage(req.StreamOptions) {
			_ = writeAndFlush(w, sseData(map[string]any{
				"id": cid, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []any{},
				"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
			}))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}

	var full strings.Builder
	err := s.StreamChat(r.Context(), prompt, chatImages, timeoutDuration, func(chunk string) error {
		full.WriteString(chunk)
		return nil
	})
	if err != nil {
		writeError(w, r, mapGenerationError(err))
		return
	}
	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	text := full.String()
	if toolNote != "" {
		calls, rest := parseToolCalls(text)
		if len(calls) > 0 {
			var content any
			if rest != "" {
				content = rest
			}
			writeJSON(w, 200, map[string]any{
				"id": cid, "object": "chat.completion", "created": created, "model": model,
				"choices": []any{map[string]any{
					"index": 0, "finish_reason": "tool_calls",
					"message": map[string]any{"role": "assistant", "content": content, "tool_calls": calls},
				}},
				"usage": usage,
			})
			return
		}
	}
	writeJSON(w, 200, map[string]any{
		"id": cid, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": text},
		}},
		"usage": usage,
	})
}

func mapGenerationError(err error) error {
	if err == nil {
		return nil
	}
	if engine.IsAuthError(err) {
		return httpError(401, "%s", err.Error())
	}
	if engine.IsRefusalError(err) {
		return httpError(502, "%s", err.Error())
	}
	var gen *engine.MuseGenerationError
	if errors.As(err, &gen) {
		return httpError(502, "%s", err.Error())
	}
	return err
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func responsesMessages(req *ResponsesRequest) []ChatMessage {
	var msgs []ChatMessage
	if req.Instructions != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: req.Instructions})
	}
	switch input := req.Input.(type) {
	case string:
		msgs = append(msgs, ChatMessage{Role: "user", Content: input})
	case []any:
		for _, item := range input {
			switch it := item.(type) {
			case string:
				msgs = append(msgs, ChatMessage{Role: "user", Content: it})
			case map[string]any:
				if role, ok := it["role"].(string); ok && role != "" {
					msgs = append(msgs, ChatMessage{Role: role, Content: it["content"]})
					continue
				}
				typ, _ := it["type"].(string)
				switch typ {
				case "input_text":
					msgs = append(msgs, ChatMessage{Role: "user", Content: it["text"]})
				case "function_call_output":
					msgs = append(msgs, ChatMessage{Role: "user", Content: "[Tool result]\n" + fmt.Sprint(it["output"])})
				case "function_call":
					msgs = append(msgs, ChatMessage{Role: "assistant", Content: "[Requested tool calls]" + fmt.Sprint(it["name"])})
				case "input_image":
					if img := chatContentImages([]ChatMessage{{Content: []any{it}}}); len(img) > 0 {
						msgs = append(msgs, ChatMessage{Role: "user", Content: []any{it}})
					}
				}
			}
		}
	}
	return msgs
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req ResponsesRequest
	if err := decodeJSONBody(w, r, &req, 64<<20); err != nil {
		writeError(w, r, err)
		return
	}
	messages := responsesMessages(&req)
	prompt := buildChatPrompt(messages)
	if strings.TrimSpace(prompt) == "" {
		writeError(w, r, httpError(400, "input is empty"))
		return
	}
	responseImages, ok := s.prepareChatImages(w, r, messages)
	if !ok {
		return
	}
	model := resolveModel(req.Model, "muse-spark")
	timeout := s.cfg.ChatTimeout
	if req.Timeout != nil && *req.Timeout > 0 {
		timeout = *req.Timeout
	}
	timeoutDuration := time.Duration(timeout) * time.Second
	rid := "resp_" + randomHex(24)
	mid := "msg_" + randomHex(24)
	created := time.Now().Unix()

	envelope := func(status, text string) map[string]any {
		done := status == "completed"
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{"type": "output_text", "text": text, "annotations": []any{}})
		}
		itemStatus := "in_progress"
		if done {
			itemStatus = "completed"
		}
		return map[string]any{
			"id": rid, "object": "response", "created_at": created, "status": status, "model": model,
			"output": []any{map[string]any{
				"type": "message", "id": mid, "role": "assistant", "status": itemStatus, "content": content,
			}},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		}
	}

	if req.Stream {
		writeSSEHeaders(w)
		w.WriteHeader(200)
		_ = writeAndFlush(w, sseEvent("response.created", map[string]any{"type": "response.created", "response": envelope("in_progress", "")}))
		_ = writeAndFlush(w, sseEvent("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"id": mid, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		}))
		_ = writeAndFlush(w, sseEvent("response.content_part.added", map[string]any{
			"type": "response.content_part.added", "item_id": mid, "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		}))
		var full strings.Builder
		err := s.StreamChat(r.Context(), prompt, responseImages, timeoutDuration, func(chunk string) error {
			full.WriteString(chunk)
			return writeAndFlush(w, sseEvent("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "item_id": mid, "output_index": 0,
				"content_index": 0, "delta": chunk,
			}))
		})
		if err != nil {
			if r.Context().Err() == nil {
				_ = writeAndFlush(w, sseEvent("response.failed", map[string]any{"type": "response.failed", "response": envelope("failed", "")}))
			}
			return
		}
		text := full.String()
		_ = writeAndFlush(w, sseEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": mid, "output_index": 0, "content_index": 0, "text": text,
		}))
		_ = writeAndFlush(w, sseEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{"id": mid, "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
		}))
		_ = writeAndFlush(w, sseEvent("response.completed", map[string]any{"type": "response.completed", "response": envelope("completed", text)}))
		return
	}

	var full strings.Builder
	err := s.StreamChat(r.Context(), prompt, responseImages, timeoutDuration, func(chunk string) error {
		full.WriteString(chunk)
		return nil
	})
	if err != nil {
		writeError(w, r, mapGenerationError(err))
		return
	}
	writeJSON(w, 200, envelope("completed", full.String()))
}
