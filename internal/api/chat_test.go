package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
)

func TestChatNonStreaming(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		if !strings.Contains(prompt, "hello") {
			t.Fatalf("prompt not forwarded: %s", prompt)
		}
		return emit("Hello from the fake model")
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Model != "muse-spark" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hello from the fake model" || resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("unexpected chat response: %s", rec.Body.String())
	}
}

func TestChatStreaming(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		if err := emit("Hi"); err != nil {
			return err
		}
		return emit(" there")
	}
	reqBody := map[string]any{
		"messages":       []any{map[string]any{"role": "user", "content": "hello"}},
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, _ := json.Marshal(reqBody)
	req, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("stream status: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("unexpected content type: %s", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hi"`, `"finish_reason":"stop"`, `"usage"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `"content":"Hi there"`) {
		t.Fatalf("stream should pace chunks separately")
	}
}

func TestChatToolProtocol(t *testing.T) {
	s, cfg := newTestServer(t, "")
	cfg.ToolProtocol = true
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		if !strings.Contains(prompt, "get_weather") {
			t.Fatalf("tool definitions were not injected: %s", prompt)
		}
		return emit("```json\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}\n```")
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/chat/completions", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "weather?"}},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "get_weather",
				"description": "Look up the weather",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
			},
		}},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("tool chat: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("expected tool_calls finish: %s", rec.Body.String())
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "get_weather" || !strings.Contains(calls[0].Function.Arguments, "Paris") {
		t.Fatalf("unexpected tool calls: %s", rec.Body.String())
	}
}

func TestResponsesStreaming(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		return emit("response text")
	}
	raw, _ := json.Marshal(map[string]any{"input": "hello", "stream": true})
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("responses stream: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed", "response text"} {
		if !strings.Contains(body, want) {
			t.Fatalf("responses stream missing %q:\n%s", want, body)
		}
	}
}

func TestVideoTaskFlow(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.runGenerationFn = func(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
		if kind != "video" {
			t.Fatalf("expected video kind, got %s", kind)
		}
		if onAcquired != nil {
			onAcquired()
		}
		if onProgress != nil {
			onProgress(40)
		}
		return &engine.GenResult{Filename: "clip.mp4", Size: 12345, Kind: "video"}, "acc-1", nil
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/videos", map[string]any{
		"prompt": "leaves", "duration": 5, "size": "16:9",
	}, true)
	if rec.Code != 200 {
		t.Fatalf("create video: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &created)
	if created.ID == "" {
		t.Fatalf("missing video task id")
	}
	deadline := time.Now().Add(3 * time.Second)
	var polled struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		Video  struct {
			URL string `json:"url"`
		} `json:"video"`
		Result struct {
			Bytes int64  `json:"bytes"`
			URL   string `json:"url"`
		} `json:"result"`
	}
	for time.Now().Before(deadline) {
		rec = doRequest(t, s, http.MethodGet, "/v1/videos/"+created.ID, nil, true)
		decodeJSON(t, rec, &polled)
		if polled.Status == "completed" || polled.Status == "failed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if polled.Status != "completed" || !strings.HasSuffix(polled.URL, "/v1/media/clip.mp4") {
		t.Fatalf("unexpected video task: %s", rec.Body.String())
	}
	if polled.Video.URL == "" || polled.Result.Bytes != 12345 || polled.Result.URL != polled.URL {
		t.Fatalf("video fields missing: %s", rec.Body.String())
	}
}

func TestChatImageAttachments(t *testing.T) {
	s, _ := newTestServer(t, "")
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	var got []string
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		got = images
		if !strings.Contains(prompt, "screenshot") {
			t.Fatalf("prompt not forwarded: %s", prompt)
		}
		if !strings.Contains(prompt, "[1 image(s) are attached with this message.]") {
			t.Fatalf("attachment note missing: %s", prompt)
		}
		return emit("I see it")
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/chat/completions", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is in this screenshot"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": png}},
		}}},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("chat with image: %d %s", rec.Code, rec.Body.String())
	}
	if len(got) != 1 || got[0] != png {
		t.Fatalf("prepared image not forwarded: %#v", got)
	}

	s2, _ := newTestServer(t, "")
	var gotPrompt string
	s2.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		gotPrompt = prompt
		if len(images) != 1 {
			t.Fatalf("expected 1 image, got %d", len(images))
		}
		return emit("done")
	}
	rec2 := doRequest(t, s2, http.MethodPost, "/v1/chat/completions", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": png},
		}}},
	}, true)
	if rec2.Code != 200 {
		t.Fatalf("image-only chat: %d %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(gotPrompt, "Respond to the 1 attached image(s).") {
		t.Fatalf("image-only prompt not synthesized: %s", gotPrompt)
	}
}

func TestChatImageAttachmentInvalid(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.streamChatFn = func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) error {
		t.Fatal("stream must not run when image validation fails")
		return nil
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/chat/completions", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": "file:///etc/passwd"},
		}}},
	}, true)
	if rec.Code != 400 {
		t.Fatalf("expected 400 for local file image, got %d %s", rec.Code, rec.Body.String())
	}
}
