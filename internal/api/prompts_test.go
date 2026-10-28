package api

import (
	"strings"
	"testing"
)

func TestParseCookieText(t *testing.T) {
	got := parseCookieText("hatch_sess=abc; hatch_vml=xyz\nfoo=bar")
	if got["hatch_sess"] != "abc" || got["hatch_vml"] != "xyz" || got["foo"] != "bar" {
		t.Fatalf("unexpected cookies: %#v", got)
	}
	got = parseCookieText(`{"a":"1","b":"2"}`)
	if got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("unexpected JSON cookies: %#v", got)
	}
	if len(parseCookieText("")) != 0 {
		t.Fatalf("empty input should produce no cookies")
	}
}

func TestParseBatch(t *testing.T) {
	entries := parseBatch("# comment\n\nmuse-01 | hatch_sess=a; hatch_vml=b\nhatch_sess=c|with=pipe\nbroken")
	if len(entries) != 2 {
		t.Fatalf("expected two entries, got %d: %#v", len(entries), entries)
	}
	if entries[0].Label != "muse-01" || entries[0].Cookies["hatch_sess"] != "a" {
		t.Fatalf("unexpected first entry: %#v", entries[0])
	}
	if entries[1].Label != "" || entries[1].Cookies["hatch_sess"] != "c|with=pipe" {
		t.Fatalf("pipe inside a value must not split the label: %#v", entries[1])
	}
}

func TestReferenceListValidation(t *testing.T) {
	refs, err := referenceList([]any{"first", map[string]any{"url": "second"}}, nil, "images")
	if err != nil || len(refs) != 2 || refs[0] != "first" || refs[1] != "second" {
		t.Fatalf("unexpected refs: %#v %v", refs, err)
	}
	if _, err := referenceList([]any{nil}, nil, "images"); err == nil || !strings.Contains(err.Error(), "images[0]") {
		t.Fatalf("expected indexed validation error, got %v", err)
	}
	if _, err := referenceList([]any{"a", "b", "c", "d", "e"}, nil, "images"); err == nil || !strings.Contains(err.Error(), "at most 4") {
		t.Fatalf("expected count error, got %v", err)
	}
	if _, err := referenceList([]any{"new"}, []any{"old"}, "images"); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected conflict error, got %v", err)
	}
	// An explicitly empty images array is falsy in the upstream contract and
	// must not conflict with a legacy single-image field.
	refs, err = referenceList([]any{}, []any{"legacy"}, "images")
	if err != nil || len(refs) != 1 || refs[0] != "legacy" {
		t.Fatalf("empty images plus legacy should be accepted: %#v %v", refs, err)
	}
	// Legacy precedence: the first non-empty legacy value wins.
	refs, err = referenceList(nil, []any{"", map[string]any{"image_url": map[string]any{"url": "legacy"}}}, "images")
	if err != nil || len(refs) != 1 || refs[0] != "legacy" {
		t.Fatalf("legacy reference failed: %#v %v", refs, err)
	}
}

func TestBuildImagePrompt(t *testing.T) {
	req := &ImageRequest{Prompt: "a red fox", Size: "16:9"}
	prompt := buildImagePrompt(req)
	if !strings.Contains(prompt, "brand-new image") || !strings.Contains(prompt, "16:9") {
		t.Fatalf("unexpected prompt: %s", prompt)
	}
	req = &ImageRequest{
		Prompt:      "a red fox",
		AspectRatio: "9:16",
		Images:      []any{"data:image/png;base64,eA=="},
		Extra:       "muted palette",
	}
	prompt = buildImagePrompt(req)
	for _, want := range []string{"reference image", "9:16", "Clean image requirement", "muted palette"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q: %s", want, prompt)
		}
	}
	req = &ImageRequest{Prompt: "square", Size: "1024x1024"}
	if !strings.Contains(buildImagePrompt(req), "1:1") {
		t.Fatalf("1024x1024 should map to a square prompt")
	}
}

func TestBuildVideoPrompt(t *testing.T) {
	dur := 8
	req := &VideoRequest{
		Prompt:     "two characters running",
		Duration:   &dur,
		Size:       "16:9",
		Images:     []any{"data:image/png;base64,eA==", "data:image/png;base64,eQ=="},
		Resolution: "1080p",
	}
	prompt := buildVideoPrompt(req)
	for _, want := range []string{"2 reference images", "first as the main starting frame", "16:9", "8 seconds", "1080p"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("video prompt missing %q: %s", want, prompt)
		}
	}
	single := &VideoRequest{Prompt: "animate", Images: []any{"data:image/png;base64,eA=="}, AspectRatio: "9:16"}
	prompt = buildVideoPrompt(single)
	if !strings.Contains(prompt, "first-frame") || !strings.Contains(prompt, "9:16") || !strings.Contains(prompt, "6 seconds") {
		t.Fatalf("unexpected single-reference prompt: %s", prompt)
	}
}

func TestBuildChatPrompt(t *testing.T) {
	msgs := []ChatMessage{{Role: "user", Content: "hello"}}
	if got := buildChatPrompt(msgs); got != "hello" {
		t.Fatalf("single user turn should pass through: %q", got)
	}
	msgs = []ChatMessage{
		{Role: "system", Content: "Be terse. danger-full-access"},
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}
	got := buildChatPrompt(msgs)
	for _, want := range []string{"Background and task settings", "standard-workspace-access", "User:", "Assistant:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q: %s", want, got)
		}
	}
	msgs = []ChatMessage{
		{Role: "user", Content: "go"},
		{Role: "tool", Content: "result"},
		{Role: "assistant", ToolCalls: []any{map[string]any{"id": "1", "function": map[string]any{"name": "x"}}}},
	}
	got = buildChatPrompt(msgs)
	if !strings.Contains(got, "[Tool result]") || !strings.Contains(got, "[Requested tool calls]") {
		t.Fatalf("missing tool markers: %s", got)
	}
}

func TestToolPromptAndParsing(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "get_weather",
			"description": "Look up weather",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"city": map[string]any{"type": "string", "description": "City name"},
				},
				"required": []any{"city"},
			},
		}},
	}
	prompt := buildToolsPrompt(tools)
	for _, want := range []string{"get_weather", "Look up weather", "city (string, required)"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("tool prompt missing %q: %s", want, prompt)
		}
	}

	calls, rest := parseToolCalls("Here you go:\n```json\n{\"name\":\"get_weather\",\"arguments\":{\"city\":\"Paris\"}}\n```")
	if len(calls) != 1 || rest != "Here you go:" {
		t.Fatalf("fenced parse failed: %#v %q", calls, rest)
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || !strings.Contains(fn["arguments"].(string), "Paris") {
		t.Fatalf("unexpected call: %#v", calls[0])
	}

	calls, rest = parseToolCalls("```json\n[{\"tool\":\"a\",\"args\":{}},{\"name\":\"b\"}]\n```")
	if len(calls) != 2 || rest != "" {
		t.Fatalf("array parse failed: %#v %q", calls, rest)
	}
	calls, _ = parseToolCalls(`{"name":"bare","arguments":{}}`)
	if len(calls) != 1 {
		t.Fatalf("bare JSON parse failed: %#v", calls)
	}
	calls, rest = parseToolCalls(`I will use {"name":"embedded","arguments":{}} now`)
	if len(calls) != 1 || !strings.Contains(rest, "I will use") {
		t.Fatalf("embedded parse failed: %#v %q", calls, rest)
	}
	if calls, rest = parseToolCalls(`{"hello":"world"}`); calls != nil || rest != `{"hello":"world"}` {
		t.Fatalf("ordinary JSON must not become a tool call: %#v %q", calls, rest)
	}
}

func TestVersionAndRepoHelpers(t *testing.T) {
	if compareVersions("1.10.0", "1.9.9") <= 0 {
		t.Fatalf("version comparison failed")
	}
	if compareVersions("2.0.0", "2.0.0") != 0 {
		t.Fatalf("equal versions should compare equal")
	}
	repo := parseRepo("https://github.com/owner/project.git", "dev")
	if repo == nil || repo.Owner != "owner" || repo.Name != "project" || !strings.Contains(repo.Raw, "/dev/version.json") {
		t.Fatalf("unexpected repo parse: %+v", repo)
	}
	if parseRepo("", "main") != nil {
		t.Fatalf("empty repo must disable updates")
	}
}

func TestChatContentImages(t *testing.T) {
	png := "data:image/png;base64,AAAA"
	msgs := []ChatMessage{
		{Role: "user", Content: "plain text only"},
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "look at this"},
			map[string]any{"type": "image_url", "image_url": png},
		}},
		{Role: "user", Content: []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			map[string]any{"type": "input_image", "image_url": "https://example.com/b.png"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"b64_json": png}},
			map[string]any{"type": "image_url", "b64_json": png},
			map[string]any{"type": "text", "text": "and this"},
		}},
	}
	got := chatContentImages(msgs)
	want := []string{png, "https://example.com/a.png", "https://example.com/b.png", png, png}
	if len(got) != len(want) {
		t.Fatalf("expected %d images, got %d: %#v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("image[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if imgs := chatContentImages([]ChatMessage{{Role: "user", Content: "no images here"}}); len(imgs) != 0 {
		t.Fatalf("string content must yield no images: %#v", imgs)
	}
}
