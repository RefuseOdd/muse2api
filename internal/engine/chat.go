package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

func (w *Worker) pollChat() (int, string, bool) {
	if w.page == nil {
		return 0, "", false
	}
	raw, err := w.page.JSString(pollChatJS)
	if err != nil || raw == "" {
		return 0, "", false
	}
	var data struct {
		Cnt  int    `json:"cnt"`
		Txt  string `json:"txt"`
		Stop bool   `json:"stop"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return 0, "", false
	}
	return data.Cnt, strings.TrimSpace(data.Txt), data.Stop
}

// ChatStream sends one prompt and emits incremental assistant text.
func (w *Worker) ChatStream(ctx context.Context, cookies map[string]string, prompt string, expires map[string]int64, timeout time.Duration, accountID string, images []string, emit func(string) error) ([]ChatFile, error) {
	if _, err := w.EnsurePage(ctx, cookies, expires, accountID); err != nil {
		return nil, err
	}
	w.ResetThread(ctx, true)
	w.installBlobHook()
	refs := append([]string{}, images...)
	if len(refs) > 0 {
		if err := w.AttachImages(refs); err != nil {
			return nil, err
		}
	} else {
		w.clearAttachments()
	}
	baseAgent, baseText, _ := w.pollChat()
	if _, err := w.send(prompt); err != nil {
		return nil, err
	}

	sentStart := time.Now()
	deadline := sentStart.Add(timeout)
	firstDeadline := sentStart.Add(40 * time.Second)
	if firstDeadline.After(deadline) {
		firstDeadline = deadline
	}
	sent := ""
	last := ""
	stable := 0
	gotFirst := false

	for time.Now().Before(firstDeadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		w.pause(60 * time.Millisecond)
		cnt, cur, _ := w.pollChat()
		if cur == "" || (cnt <= baseAgent && cur == baseText) {
			if time.Since(sentStart) > 14*time.Second {
				tail, _ := w.page.JSString(`document.body.innerText.slice(-500)`)
				if strings.Contains(tail, "Still sending") || strings.Contains(tail, "Connecting...") {
					return nil, genError("cloud VM connection timed out (Still sending)")
				}
			}
			continue
		}
		delta := cur
		if strings.HasPrefix(cur, sent) {
			delta = cur[len(sent):]
		}
		if delta != "" {
			sent = cur
			if err := emit(delta); err != nil {
				return nil, err
			}
			last = cur
			gotFirst = true
			break
		}
	}
	if !gotFirst {
		return nil, genError("timed out waiting for the assistant's first token")
	}

	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		w.pause(60 * time.Millisecond)
		_, cur, hasStop := w.pollChat()
		if cur == "" {
			continue
		}
		if cur != last {
			delta := cur
			if strings.HasPrefix(cur, sent) {
				delta = cur[len(sent):]
			}
			if delta != "" {
				sent = cur
				if err := emit(delta); err != nil {
					return nil, err
				}
			}
			last = cur
			stable = 0
		} else {
			stable++
			if !hasStop && stable >= 3 {
				return w.captureChatFiles(ctx), nil
			}
		}
	}
	return nil, genError("timed out waiting for the assistant reply")
}
