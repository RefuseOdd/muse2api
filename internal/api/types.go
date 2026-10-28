package api

import (
	"fmt"
	"strings"
)

const (
	maxReferenceImages     = 4
	maxReferenceImageBytes = 10 * 1024 * 1024
	maxReferenceTotalBytes = 24 * 1024 * 1024
)

type ImageRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	AspectRatio    string `json:"aspect_ratio"`
	ResponseFormat string `json:"response_format"`
	Timeout        *int   `json:"timeout"`
	Extra          string `json:"extra"`
	Image          any    `json:"image"`
	Images         []any  `json:"images"`
	ImageURL       any    `json:"image_url"`
	ReferenceImage string `json:"reference_image"`
	Async          bool   `json:"async"`
}

type VideoRequest struct {
	Prompt         string `json:"prompt"`
	Model          string `json:"model"`
	Duration       *int   `json:"duration"`
	Size           string `json:"size"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
	Timeout        *int   `json:"timeout"`
	Extra          string `json:"extra"`
	Image          any    `json:"image"`
	Images         []any  `json:"images"`
	ImageURL       any    `json:"image_url"`
	ReferenceImage string `json:"reference_image"`
}

type ChatMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	Name       string `json:"name"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []any  `json:"tool_calls"`
}

type ChatRequest struct {
	Model          string        `json:"model"`
	Messages       []ChatMessage `json:"messages"`
	Stream         bool          `json:"stream"`
	Temperature    *float64      `json:"temperature"`
	MaxTokens      *int          `json:"max_tokens"`
	Timeout        *int          `json:"timeout"`
	Prompt         string        `json:"prompt"`
	Tools          []any         `json:"tools"`
	ToolChoice     any           `json:"tool_choice"`
	ResponseFormat any           `json:"response_format"`
	StreamOptions  any           `json:"stream_options"`
}

type ResponsesRequest struct {
	Model           string `json:"model"`
	Input           any    `json:"input"`
	Instructions    string `json:"instructions"`
	Stream          bool   `json:"stream"`
	MaxOutputTokens *int   `json:"max_output_tokens"`
	Timeout         *int   `json:"timeout"`
	Tools           []any  `json:"tools"`
	Store           *bool  `json:"store"`
}

type AccountRequest struct {
	Label        string            `json:"label"`
	Cookies      map[string]string `json:"cookies"`
	CookieHeader string            `json:"cookie_header"`
	Batch        string            `json:"batch"`
	Expires      map[string]int64  `json:"expires"`
}

type AccountPatch struct {
	Label   *string `json:"label"`
	Enabled *bool   `json:"enabled"`
}

var models = []map[string]any{
	{"id": "muse-spark", "object": "model", "owned_by": "muse",
		"description": "Muse Spark - text and code chat (web free quota, streaming supported)"},
	{"id": "muse-image", "object": "model", "owned_by": "muse",
		"description": "Muse Image - text-to-image and image editing (web free quota)"},
	{"id": "muse-video", "object": "model", "owned_by": "muse",
		"description": "Muse Video - text-to-video and image-to-video (web free quota)"},
}

var modelAliases = map[string]string{
	"muse-text": "muse-spark", "muse-chat": "muse-spark", "muse-llm": "muse-spark",
	"koda":          "muse-spark",
	"gpt-3.5-turbo": "muse-spark", "gpt-4": "muse-spark", "gpt-4-turbo": "muse-spark",
	"gpt-4o": "muse-spark", "gpt-4o-mini": "muse-spark", "gpt-4.1": "muse-spark",
	"gpt-4.1-mini": "muse-spark", "gpt-5": "muse-spark", "gpt-5-codex": "muse-spark",
	"o1": "muse-spark", "o1-mini": "muse-spark", "o3": "muse-spark",
	"o3-mini": "muse-spark", "o4-mini": "muse-spark",
	"codex": "muse-spark", "codex-mini-latest": "muse-spark",
	"claude-3-5-sonnet": "muse-spark", "claude-3-5-sonnet-latest": "muse-spark",
	"claude-3-7-sonnet": "muse-spark", "claude-sonnet-4": "muse-spark",
	"claude-opus-4": "muse-spark", "claude-3-opus": "muse-spark",
	"claude-3-haiku": "muse-spark",
	"deepseek-chat":  "muse-spark", "deepseek-coder": "muse-spark",
	"deepseek-reasoner": "muse-spark", "qwen-coder": "muse-spark",
	"gemini-2.5-pro": "muse-spark", "gemini-2.5-flash": "muse-spark",
	"muse-img": "muse-image", "dall-e": "muse-image", "dall-e-3": "muse-image",
	"gpt-image-1": "muse-image", "flux": "muse-image", "midjourney": "muse-image",
	"muse-vid": "muse-video", "muse-videos": "muse-video",
	"sora": "muse-video", "sora-2": "muse-video", "veo": "muse-video",
	"veo-3": "muse-video", "kling": "muse-video", "runway": "muse-video",
}

func resolveModel(name, fallback string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return fallback
	}
	if resolved, ok := modelAliases[n]; ok {
		return resolved
	}
	return n
}

func imageValue(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		var candidate any
		for _, key := range []string{"image_url", "url", "b64_json"} {
			if candidate != nil {
				break
			}
			candidate = v[key]
		}
		if nested, ok := candidate.(map[string]any); ok {
			candidate = nested["url"]
		}
		if s, ok := candidate.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func referenceList(images []any, legacy []any, field string) ([]string, error) {
	primary := ""
	for _, value := range legacy {
		if ref := imageValue(value); ref != "" {
			primary = ref
			break
		}
	}
	if primary != "" && len(images) > 0 {
		return nil, &HTTPError{Status: 400, Message: field + " cannot be combined with legacy single-image fields"}
	}
	if primary != "" {
		return []string{primary}, nil
	}
	if images == nil {
		return nil, nil
	}
	if len(images) > maxReferenceImages {
		return nil, &HTTPError{Status: 400, Message: fmt.Sprintf("%s supports at most %d reference images", field, maxReferenceImages)}
	}
	refs := make([]string, 0, len(images))
	for index, value := range images {
		ref := imageValue(value)
		if ref == "" {
			return nil, &HTTPError{Status: 400, Message: fmt.Sprintf("%s[%d] is not a valid image reference", field, index)}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (r *ImageRequest) references() ([]string, error) {
	return referenceList(r.Images, []any{r.ReferenceImage, r.Image}, "images")
}

func (r *VideoRequest) references() ([]string, error) {
	return referenceList(r.Images, []any{r.ReferenceImage, r.ImageURL, r.Image}, "images")
}

func contentText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			switch it := item.(type) {
			case map[string]any:
				typ, _ := it["type"].(string)
				switch typ {
				case "", "text", "input_text", "output_text":
					if s, ok := it["text"].(string); ok {
						parts = append(parts, s)
					}
				case "image_url":
					parts = append(parts, "[image]")
				}
			case string:
				parts = append(parts, it)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprint(v)
	}
}

func chatContentImages(messages []ChatMessage) []string {
	var images []string
	for _, m := range messages {
		parts, ok := m.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range parts {
			it, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := it["type"].(string)
			if typ != "image_url" && typ != "input_image" {
				continue
			}
			ref := imageValue(it["image_url"])
			if ref == "" {
				ref = imageValue(it)
			}
			if ref != "" {
				images = append(images, ref)
			}
		}
	}
	return images
}

func buildChatPrompt(messages []ChatMessage) string {
	var system []string
	type turn struct {
		role string
		text string
	}
	var turns []turn
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := strings.TrimSpace(contentText(m.Content))
		if role == "tool" {
			if text != "" {
				turns = append(turns, turn{"user", "[Tool result]\n" + text})
			}
			continue
		}
		if text == "" {
			if role == "assistant" && len(m.ToolCalls) > 0 {
				raw, _ := jsonMarshalNoEscape(m.ToolCalls)
				if len(raw) > 600 {
					raw = raw[:600]
				}
				turns = append(turns, turn{"assistant", "[Requested tool calls]" + raw})
			}
			continue
		}
		if role == "system" || role == "developer" {
			system = append(system, text)
		} else {
			turns = append(turns, turn{role, text})
		}
	}
	if len(turns) == 1 && len(system) == 0 && turns[0].role == "user" {
		return turns[0].text
	}
	var parts []string
	if len(system) > 0 {
		sysText := strings.Join(system, "\n\n")
		sysText = strings.ReplaceAll(sysText, "danger-full-access", "standard-workspace-access")
		parts = append(parts, "Background and task settings:\n"+sysText)
	}
	for _, t := range turns {
		label := "User"
		if t.role == "assistant" {
			label = "Assistant"
		}
		parts = append(parts, label+":\n"+t.text)
	}
	return strings.Join(parts, "\n\n")
}

func buildImagePrompt(r *ImageRequest) string {
	refs, _ := r.references()
	hasRef := len(refs) > 0
	var parts []string
	if hasRef {
		parts = append(parts, "Create or edit an image based on the reference image(s) I uploaded with this message: "+strings.TrimSpace(r.Prompt))
	} else {
		parts = append(parts, "Create a brand-new image from text only (no reference image was provided; do not search the photo library or ask for an original, draw a completely new image from the description): "+strings.TrimSpace(r.Prompt))
	}
	ar := strings.ToLower(strings.TrimSpace(r.AspectRatio))
	sz := strings.ToLower(strings.TrimSpace(r.Size))
	combined := ar + " " + sz
	switch {
	case containsAny(combined, "9:16", "9/16", "portrait", "720x1280", "1080x1920"):
		parts = append(parts, "[Framing and aspect ratio]: strictly 9:16 vertical portrait full-screen (9:16 vertical portrait aspect ratio, height greater than width), never generate landscape, keep a vertical composition")
	case containsAny(combined, "16:9", "16/9", "landscape", "1280x720", "1920x1080"):
		parts = append(parts, "[Framing and aspect ratio]: 16:9 widescreen landscape (16:9 widescreen landscape aspect ratio)")
	case containsAny(combined, "1:1", "square", "1024x1024"):
		parts = append(parts, "[Framing and aspect ratio]: 1:1 square (1:1 square aspect ratio)")
	case containsAny(combined, "4:3", "4/3"):
		parts = append(parts, "[Framing and aspect ratio]: 4:3 aspect ratio")
	case containsAny(combined, "3:4", "3/4"):
		parts = append(parts, "[Framing and aspect ratio]: 3:4 vertical aspect ratio")
	case r.AspectRatio != "":
		parts = append(parts, "[Framing and aspect ratio]: "+r.AspectRatio+" aspect ratio")
	case r.Size != "":
		parts = append(parts, "Size/aspect: "+r.Size)
	}
	if hasRef {
		parts = append(parts, "[Clean image requirement]: completely remove all text, watermarks, signatures, corner marks and logos from the reference image (clean image without any watermark, text, or logo); output a clean image with no lettering")
	}
	if r.Extra != "" {
		parts = append(parts, r.Extra)
	}
	return strings.Join(parts, ", ")
}

func buildVideoPrompt(r *VideoRequest) string {
	refs, _ := r.references()
	hasRef := len(refs) > 0
	dur := 6
	if r.Duration != nil && *r.Duration > 0 {
		dur = *r.Duration
	}
	referenceInstruction := "Use the reference image I just uploaded as the first-frame reference (never use historical images or any other image; the starting frame must be exactly this image)"
	if len(refs) > 1 {
		referenceInstruction = fmt.Sprintf("Create a video based on the %d reference images I uploaded with this message (use the first as the main starting frame and the rest as subject, scene and style references; never use historical images or other images)", len(refs))
	}
	ar := strings.ToLower(strings.TrimSpace(r.AspectRatio))
	sz := strings.ToLower(strings.TrimSpace(r.Size))
	combined := ar + " " + sz
	vertical := containsAny(combined, "9:16", "9/16", "portrait", "720x1280", "1080x1920")
	landscape := containsAny(combined, "16:9", "16/9", "landscape", "1280x720", "1920x1080")
	var intro string
	switch {
	case vertical && hasRef:
		intro = fmt.Sprintf("%s: create a strictly 9:16 vertical portrait full-screen image-to-video clip (9:16 vertical portrait video, 720x1280, height greater than width, no landscape and no black bars, strictly %d seconds): %s", referenceInstruction, dur, strings.TrimSpace(r.Prompt))
	case vertical:
		intro = fmt.Sprintf("Create a brand-new text-to-video clip (text only; never reference historical images or context): create a strictly 9:16 vertical portrait full-screen video (9:16 vertical portrait video, 720x1280, height greater than width, no landscape and no side black bars, keep vertical composition, strictly %d seconds): %s", dur, strings.TrimSpace(r.Prompt))
	case landscape && hasRef:
		intro = fmt.Sprintf("%s: create a 16:9 widescreen landscape image-to-video clip (16:9 widescreen landscape video, strictly %d seconds): %s", referenceInstruction, dur, strings.TrimSpace(r.Prompt))
	case landscape:
		intro = fmt.Sprintf("Create a brand-new text-to-video clip (text only; never reference historical images or context): create a 16:9 widescreen landscape video (16:9 widescreen landscape video, strictly %d seconds): %s", dur, strings.TrimSpace(r.Prompt))
	case hasRef:
		intro = fmt.Sprintf("%s: create a dynamic image-to-video clip (strictly %d seconds): %s", referenceInstruction, dur, strings.TrimSpace(r.Prompt))
	default:
		intro = fmt.Sprintf("Create a brand-new text-to-video clip (text only; never reference historical images or context): create a video (strictly %d seconds): %s", dur, strings.TrimSpace(r.Prompt))
	}
	parts := []string{intro}
	if r.Resolution != "" {
		parts = append(parts, "Quality: "+r.Resolution)
	}
	if r.Extra != "" {
		parts = append(parts, r.Extra)
	}
	return strings.Join(parts, ", ")
}

func containsAny(s string, values ...string) bool {
	for _, v := range values {
		if strings.Contains(s, v) {
			return true
		}
	}
	return false
}
