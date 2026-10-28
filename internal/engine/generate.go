package engine

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	creditRE      = regexp.MustCompile(`(?i)额度不足|积分不足|out of credits|达到上限|token limit`)
	undeliveredRE = regexp.MustCompile(`(?i)Still sending|Delivery not confirmed|正在发送|未确认送达`)
	mediaReportRE = regexp.MustCompile(`(?i)\.(?:webp|png|jpe?g|mp4|webm)|imagine_media|deliverable|generated\s+.*image|verified\s+generated|artifact`)
)

// Attachments returns the currently visible generated media containers. Input
// previews, the composer and user bubbles are excluded by the selector script.
func (w *Worker) Attachments() []Attachment {
	if w.page == nil {
		return nil
	}
	raw, err := w.page.JSString(attachmentsJS)
	if err != nil || raw == "" {
		return nil
	}
	var out []Attachment
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// MatchAttachment applies the strict per-modality acceptance rules. A video
// request never accepts a still cover image, even when the test id says video.
func MatchAttachment(a Attachment, expect string) bool {
	tid := strings.ToLower(a.TID)
	if expect == "video" {
		return a.HasVideo || a.VSrc != "" || hasVideoExt(a.Src)
	}
	return strings.Contains(tid, "image") || (!a.HasVideo && !hasVideoExt(a.Src))
}

func hasVideoExt(src string) bool {
	lower := strings.ToLower(src)
	return strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".webm") || strings.HasSuffix(lower, ".mov")
}

func (w *Worker) send(prompt string) (string, error) {
	if w.page == nil {
		return "", genError("page is not available")
	}
	_, _ = w.page.JSString(focusTextareaJS)
	w.pause(100 * time.Millisecond)

	rectRaw, _ := w.page.JSString(textareaRectJS)
	if rectRaw == "" || rectRaw == "null" {
		return "", genError("could not find the chat input box")
	}
	var rect struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := json.Unmarshal([]byte(rectRaw), &rect); err != nil {
		return "", genError("could not measure the chat input box")
	}
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		_, _ = w.page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": rect.X, "y": rect.Y, "button": "left", "clickCount": 1,
		}, 30*time.Second)
	}
	w.pause(100 * time.Millisecond)

	promptJSON, _ := json.Marshal(prompt)
	setExpr := fmt.Sprintf(setTextareaValueJS, string(promptJSON))
	val, _ := w.page.JS(setExpr)
	length := 0
	switch v := val.(type) {
	case float64:
		length = int(v)
	case int:
		length = v
	}
	if length == 0 && len(prompt) < 500 {
		_, _ = w.page.Send("Input.insertText", map[string]any{"text": prompt}, 30*time.Second)
		_, _ = w.page.JS(setExpr)
	}

	clicked := "no-button"
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		res, _ := w.page.JSString(clickSendJS)
		if res == "clicked" {
			clicked = "clicked"
			break
		}
		w.pause(80 * time.Millisecond)
	}
	if clicked != "clicked" {
		for _, modifiers := range []int{1, 0} {
			for _, typ := range []string{"keyDown", "char", "keyUp"} {
				params := map[string]any{
					"type": typ, "key": "Enter", "code": "Enter",
					"windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13,
					"modifiers": modifiers,
				}
				if typ == "char" {
					params["text"] = "\r"
					params["unmodifiedText"] = "\r"
				}
				_, _ = w.page.Send("Input.dispatchKeyEvent", params, 30*time.Second)
			}
			w.pause(time.Second)
			empty, _ := w.page.JSString(textareaEmptyJS)
			if empty == "true" {
				clicked = "enter-sent"
				break
			}
		}
	}
	w.pause(300 * time.Millisecond)
	if err := w.page.context().Err(); err != nil {
		return "", err
	}
	return clicked, nil
}

func (w *Worker) WaitAttachment(ctx context.Context, baselineSrc string, timeout time.Duration, expect string, onProgress func(int), baseAgentCnt, baseAttCnt int, baselineSources map[string]bool) (*Attachment, error) {
	baseline := map[string]bool{}
	for k, v := range baselineSources {
		if v {
			baseline[k] = true
		}
	}
	if baselineSrc != "" {
		baseline[baselineSrc] = true
	}
	start := time.Now()
	deadline := start.Add(timeout)
	stableSrc := ""
	stableN := 0
	lastTxt := ""
	txtStable := 0
	refusalLast := ""
	refusalStable := 0
	seenStop := false
	lastActivity := start
	progressSent := 0
	var previousState string

	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		w.pause(600 * time.Millisecond)
		w.ScrollBottom()
		atts := w.Attachments()

		var matching []Attachment
		for _, a := range atts {
			if MatchAttachment(a, expect) {
				matching = append(matching, a)
			}
		}
		var att *Attachment
		if len(matching) > 0 {
			att = &matching[len(matching)-1]
		}
		if att != nil {
			checkSrc := ""
			if expect == "video" {
				if att.VSrc != "" {
					checkSrc = att.VSrc
				} else if hasVideoExt(att.Src) {
					checkSrc = att.Src
				}
			} else {
				checkSrc = att.Src
			}
			if checkSrc != "" && !baseline[checkSrc] {
				if att.W > 0 && att.H > 0 {
					return att, nil
				}
				if checkSrc == stableSrc {
					stableN++
				} else {
					stableSrc, stableN = checkSrc, 0
				}
				if stableN >= 1 {
					return att, nil
				}
			}
		}

		elapsed := time.Since(start)
		stateRaw, _ := w.page.JSString(waitStateJS)
		var state struct {
			Cnt  int    `json:"cnt"`
			Txt  string `json:"txt"`
			Stop bool   `json:"stop"`
			Tail string `json:"tail"`
			Path string `json:"path"`
		}
		if stateRaw != "" {
			_ = json.Unmarshal([]byte(stateRaw), &state)
		}
		seenStop = seenStop || state.Stop
		stateKey := fmt.Sprintf("%d|%s|%d|%v|%s", state.Cnt, state.Txt, len(atts), state.Stop, state.Path)
		if previousState == "" || stateKey != previousState {
			lastActivity = time.Now()
			previousState = stateKey
		}

		if onProgress != nil {
			milestone := 20
			if seenStop {
				milestone = 25
			}
			if state.Cnt > baseAgentCnt {
				milestone = 40
			}
			if milestone > progressSent {
				onProgress(milestone)
				progressSent = milestone
			}
		}

		if creditRE.MatchString(state.Tail) {
			return nil, genError("account quota is exhausted")
		}
		if elapsed > 30*time.Second && state.Cnt == baseAgentCnt && undeliveredRE.MatchString(state.Tail) {
			return nil, genError("the prompt was not delivered to the backend (Still sending / Delivery not confirmed); stopped waiting and will retry")
		}

		refusalSource := ""
		if state.Cnt > baseAgentCnt {
			refusalSource = state.Txt
		} else if seenStop && !state.Stop && elapsed > 20*time.Second {
			refusalSource = state.Tail
		}
		refusal := MediaRefusalText(refusalSource, expect)
		if refusal != "" && !state.Stop && len(atts) <= baseAttCnt {
			if refusal == refusalLast {
				refusalStable++
			} else {
				refusalLast, refusalStable = refusal, 0
			}
			if refusalStable >= 4 && elapsed > 8*time.Second {
				return nil, refusalError("the model returned text instead of media: %s", refusal)
			}
		} else {
			refusalLast, refusalStable = "", 0
		}

		if state.Cnt > baseAgentCnt && state.Txt != "" && !state.Stop && len(atts) <= baseAttCnt {
			if !mediaReportRE.MatchString(state.Txt) {
				if state.Txt == lastTxt {
					txtStable++
				} else {
					lastTxt, txtStable = state.Txt, 0
				}
				if txtStable >= 15 && elapsed > 8*time.Second {
					return nil, refusalError("the model returned text instead of media: %s", CleanModelText(state.Txt))
				}
			} else {
				txtStable = 0
			}
		} else {
			txtStable = 0
		}

		if expect == "video" && seenStop && !state.Stop &&
			strings.HasSuffix(strings.TrimRight(state.Path, "/"), "/thread/new") &&
			state.Cnt <= baseAgentCnt && len(atts) <= baseAttCnt &&
			elapsed >= 180*time.Second && time.Since(lastActivity) >= 90*time.Second {
			return nil, genError("Muse did not create a video task thread; the page stayed idle after submission (stopped early)")
		}
	}
	return nil, nil
}

// CleanModelText collapses whitespace and truncates diagnostic model text.
func CleanModelText(text string) string {
	clean := strings.Join(strings.Fields(text), " ")
	if len(clean) > 240 {
		clean = clean[:240]
	}
	return clean
}

var refusalPatternsVideo = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:没生成成|视频没生成成功|视频未生成成功|视频生成失败)[^\n]{0,240}(?:国家|地区|country|region)[^\n]{0,160}(?:不支持|不可用|无法|not available|unavailable|not supported)`),
	regexp.MustCompile(`(?i)^(?:could not|couldn't|cannot|can't|unable to)[^\n]{0,120}video[^\n]{0,160}(?:country|region|not available|unavailable|not supported)`),
	regexp.MustCompile(`(?i)^(?:video (?:not|was not|wasn't) (?:generated|created|available)|video generation failed)[^\n]{0,240}(?:country|region|not available|unavailable|not supported)`),
}

var refusalPatternsImage = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:图片没生成成功|图片未生成成功|图片生成失败|无法生成图片)`),
	regexp.MustCompile(`(?i)^(?:could not|couldn't|cannot|can't|unable to)[^\n]{0,120}image`),
}

// MediaRefusalText returns only an explicit refusal line, never an arbitrary
// page tail.
func MediaRefusalText(text, expect string) string {
	if text == "" {
		return ""
	}
	patterns := refusalPatternsImage
	if expect == "video" {
		patterns = refusalPatternsVideo
	}
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		clean := CleanModelText(lines[i])
		if clean == "" {
			continue
		}
		for _, pattern := range patterns {
			if pattern.MatchString(clean) {
				return clean
			}
		}
	}
	return ""
}

func (w *Worker) extractBytes(ctx context.Context, src, expect string) ([]byte, string, string, error) {
	lastErr := "unknown"
	for attempt := 0; attempt < 4; attempt++ {
		select {
		case <-ctx.Done():
			return nil, "", "", ctx.Err()
		default:
		}
		srcJSON, _ := json.Marshal(src)
		expectJSON, _ := json.Marshal(expect)
		expr := fmt.Sprintf(extractBytesJS, string(srcJSON), string(expectJSON))
		raw, err := w.page.JSAwait(expr, remaining(ctx, 600*time.Second))
		var info struct {
			OK   bool   `json:"ok"`
			Err  string `json:"err"`
			MIME string `json:"mime"`
			URL  string `json:"url"`
			B64  string `json:"b64"`
		}
		if err == nil && raw != "" {
			_ = json.Unmarshal([]byte(raw), &info)
		}
		if info.OK {
			data, derr := base64.StdEncoding.DecodeString(info.B64)
			if derr == nil {
				return data, info.MIME, info.URL, nil
			}
			lastErr = derr.Error()
		} else if info.Err != "" {
			lastErr = info.Err
		} else if err != nil {
			lastErr = err.Error()
		}
		w.pause(2 * time.Second)
	}
	return nil, "", "", genError("failed to retrieve the generated result: %s", lastErr)
}

func (w *Worker) downloadFallback(ctx context.Context, src string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	before := map[string]bool{}
	if entries, err := os.ReadDir(w.page.downloadDir); err == nil {
		for _, e := range entries {
			before[e.Name()] = true
		}
	}
	srcJSON, _ := json.Marshal(src)
	res, _ := w.page.JSString(fmt.Sprintf(downloadScopedJS, string(srcJSON)))
	if res != "ok" {
		return ""
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		select {
		case <-ctx.Done():
			return ""
		default:
		}
		pause(ctx, 1500*time.Millisecond)
		entries, err := os.ReadDir(w.page.downloadDir)
		if err != nil {
			continue
		}
		type candidate struct {
			path string
			mod  time.Time
		}
		var newFiles []candidate
		for _, e := range entries {
			name := e.Name()
			if before[name] || strings.HasSuffix(name, ".crdownload") {
				continue
			}
			p := filepath.Join(w.page.downloadDir, name)
			st, err := os.Stat(p)
			if err == nil {
				newFiles = append(newFiles, candidate{path: p, mod: st.ModTime()})
			}
		}
		sort.SliceStable(newFiles, func(i, j int) bool { return newFiles[i].mod.After(newFiles[j].mod) })
		if len(newFiles) > 0 {
			if st, err := os.Stat(newFiles[0].path); err == nil && st.Size() > 0 {
				return newFiles[0].path
			}
		}
	}
	return ""
}

func (w *Worker) clearAttachments() {
	if w.page == nil {
		return
	}
	_, _ = w.page.JSString(clearAttachmentsJS)
	w.pause(300 * time.Millisecond)
}

// AttachImages uploads ordered reference images and waits until every one is
// confirmed inside the composer.
func (w *Worker) AttachImages(images []string) error {
	if len(images) > MaxReferenceImages {
		return genError("at most %d reference images are supported", MaxReferenceImages)
	}
	type payload struct {
		B64   string `json:"b64"`
		MIME  string `json:"mime"`
		Index int    `json:"index"`
	}
	items := make([]payload, 0, len(images))
	total := 0
	for index, image := range images {
		if image == "" {
			continue
		}
		b64, mime, err := w.eng.NormalizeImageContext(w.page.context(), image)
		if err != nil {
			return err
		}
		if b64 == "" {
			return genError("reference image %d could not be read; generation stopped", index+1)
		}
		data, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			return genError("reference image %d has invalid encoding", index+1)
		}
		total += len(data)
		if total > MaxReferenceTotalBytes {
			return genError("reference images exceed the 24 MiB total limit")
		}
		items = append(items, payload{B64: b64, MIME: mime, Index: index + 1})
	}
	if len(items) == 0 {
		return nil
	}
	w.clearAttachments()
	payloadJSON, _ := json.Marshal(items)
	expr := fmt.Sprintf(attachImagesJS, string(payloadJSON))
	timeout := time.Duration(max(15, len(items)*3)) * time.Second
	raw, err := w.page.JSAwait(expr, timeout)
	if err != nil {
		return genError("failed to attach reference images; generation stopped")
	}
	var result struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if json.Unmarshal([]byte(raw), &result) != nil || !result.OK || result.Count != len(items) {
		return genError("failed to attach reference images; generation stopped")
	}

	deadline := time.Now().Add(15 * time.Second)
	attached := 0
	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		val, _ := w.page.JS(composerAttachedCountJS)
		if f, ok := val.(float64); ok {
			attached = int(f)
		}
		if attached >= len(items) {
			break
		}
		w.pause(300 * time.Millisecond)
	}
	if attached < len(items) {
		return genError("reference image upload was not confirmed (%d/%d); generation stopped", attached, len(items))
	}
	w.pause(500 * time.Millisecond)
	return nil
}

func (w *Worker) Generate(ctx context.Context, cookies map[string]string, prompt, expect string, timeout time.Duration, expires map[string]int64, accountID string, onProgress func(int), referenceImage string, referenceImages []string) (*GenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if _, err := w.EnsurePage(ctx, cookies, expires, accountID); err != nil {
		return nil, err
	}
	w.ResetThread(ctx, false)
	w.ScrollBottom()

	refs := append([]string{}, referenceImages...)
	if len(refs) == 0 && referenceImage != "" {
		refs = []string{referenceImage}
	}
	if len(refs) > 0 {
		if err := w.AttachImages(refs); err != nil {
			return nil, err
		}
	} else {
		w.clearAttachments()
	}
	attsBefore := w.Attachments()
	baselineSources := map[string]bool{}
	for _, a := range attsBefore {
		for _, s := range []string{a.Src, a.VSrc, a.ISrc} {
			if s != "" {
				baselineSources[s] = true
			}
		}
	}
	baselineSrc := ""
	if len(attsBefore) > 0 {
		baselineSrc = attsBefore[len(attsBefore)-1].Src
	}
	agentVal, _ := w.page.JS(agentCountJS)
	baseAgentCnt := 0
	if f, ok := agentVal.(float64); ok {
		baseAgentCnt = int(f)
	}
	if time.Now().After(deadline) {
		return nil, genError("task preparation timed out before sending")
	}
	sent, err := w.send(prompt)
	if err != nil {
		return nil, err
	}
	if sent != "clicked" && sent != "enter-sent" {
		return nil, genError("prompt send was not confirmed; generation stopped")
	}
	att, err := w.WaitAttachment(ctx, baselineSrc, time.Until(deadline), expect, onProgress, baseAgentCnt, len(attsBefore), baselineSources)
	if err != nil {
		return nil, err
	}
	if att == nil {
		w.debugDump("no-attachment")
		return nil, genError("generation timed out before a new result appeared")
	}

	_ = os.MkdirAll(w.eng.cfg.MediaDir(), 0o755)
	selectedSrc := att.Src
	if expect == "video" && att.VSrc != "" {
		selectedSrc = att.VSrc
	}
	data, mime, url, extractErr := w.extractBytes(ctx, selectedSrc, expect)
	if extractErr != nil {
		w.debugDump("extract-fail")
	}
	if len(data) > 0 {
		ext := PickExt(mime, url, expect)
		name := randomHex(16) + ext
		dst := filepath.Join(w.eng.cfg.MediaDir(), name)
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return nil, genError("failed to save generated media: %v", err)
		}
		kind := "image"
		if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
			kind = "video"
		}
		if expect == "video" && kind != "video" {
			return nil, genError("expected a video but the extracted result is an image (%s); refusing to deliver an image as a video", ext)
		}
		return &GenResult{
			Path: dst, Filename: name, Size: int64(len(data)), Ext: ext, MIME: mime,
			Kind: kind, Via: "blob", Attachment: att.TID, W: att.W, H: att.H,
		}, nil
	}

	path := w.downloadFallback(ctx, selectedSrc, 180*time.Second)
	if path == "" {
		return nil, genError("the media was generated but could not be retrieved")
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		ext = ".bin"
	}
	name := randomHex(16) + ext
	dst := filepath.Join(w.eng.cfg.MediaDir(), name)
	if err := moveFile(path, dst); err != nil {
		return nil, genError("failed to store downloaded media: %v", err)
	}
	kind := "image"
	if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
		kind = "video"
	}
	if expect == "video" && kind != "video" {
		return nil, genError("expected a video but the downloaded result is an image (%s); refusing to deliver an image as a video", ext)
	}
	st, _ := os.Stat(dst)
	var size int64
	if st != nil {
		size = st.Size()
	}
	return &GenResult{
		Path: dst, Filename: name, Size: size, Ext: ext, MIME: "",
		Kind: kind, Via: "download", Attachment: att.TID, W: att.W, H: att.H,
	}, nil
}

func PickExt(mime, url, expect string) string {
	m := strings.ToLower(mime)
	for _, pair := range [][2]string{
		{"mp4", ".mp4"}, {"webm", ".webm"}, {"png", ".png"},
		{"jpeg", ".jpg"}, {"jpg", ".jpg"}, {"webp", ".webp"}, {"gif", ".gif"},
	} {
		if strings.Contains(m, pair[0]) {
			return pair[1]
		}
	}
	lowerURL := strings.ToLower(url)
	for _, ext := range []string{".mp4", ".webm", ".png", ".jpg", ".webp"} {
		if strings.Contains(lowerURL, ext) {
			return ext
		}
	}
	if expect == "video" {
		return ".mp4"
	}
	return ".png"
}

func (w *Worker) debugDump(tag string) {
	if w.page == nil {
		return
	}
	raw, err := w.page.JSString(debugDumpJS)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(w.eng.cfg.DataDir(), "debug-"+tag+".json"), []byte(raw), 0o644)
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	return os.Remove(src)
}

func randomHex(n int) string {
	const alphabet = "0123456789abcdef"
	out := make([]byte, n)
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	for i := range out {
		out[i] = alphabet[int(buf[i])%16]
	}
	return string(out)
}
