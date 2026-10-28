package engine

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/store"
)

var (
	planRE        = regexp.MustCompile(`(?i)^.{1,60}?(?:\s+plan|計劃|计划|方案)$`)
	weeklyResetEN = regexp.MustCompile(`(?i)Weekly limit resets? on ([^\n]+)`)
	weeklyResetZH = regexp.MustCompile(`(?:每(?:週|周|星期)|週|周)(?:上限|限額|限额|額度|额度)(?:會|会|將|将)?(?:於|于|在)\s*([^\n]+?)\s*(?:重設|重置)`)
	extraMarkerRE = regexp.MustCompile(`(?i)^(?:(?:Additional|Extra)\s+tokens|(?:額外|额外)(?:的)?(?:符元|符記|代幣|代币|令牌|憑證|凭证|tokens))`)
	pctUsedRE     = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*%\s*(?:used\b|已(?:使)?用)`)
	pctUsedZHRE   = regexp.MustCompile(`已(?:使)?用\s*(\d+(?:\.\d+)?)\s*%`)
	leftParenRE   = regexp.MustCompile(`\(([^)]+)\)`)
	neverRE       = regexp.MustCompile(`(?i)Never expires|永不過期|永不过期`)
)

// Quota reads one account's usage in a fresh, disposable browser context.
func (w *Worker) Quota(ctx context.Context, cookies map[string]string, expires map[string]int64, accountID string) (*store.Quota, error) {
	w.ClosePage()
	defer w.ClosePage()
	if _, err := w.EnsurePage(ctx, cookies, expires, accountID); err != nil {
		return nil, err
	}
	if err := w.quotaClick(ctx, quotaSettingsJS, "could not find the settings entry (Settings / 設定 / 设置)"); err != nil {
		return nil, err
	}
	if err := w.quotaClick(ctx, quotaMenuJS, "the settings menu did not show a recognizable Settings item"); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		text, _ := w.page.JSString(quotaDialogJS)
		parsed := ParseQuota(text)
		if parsed.Found {
			return parsed, nil
		}
		if time.Now().After(deadline) {
			return nil, genError("the usage panel did not return valid quota data; the previous cache was preserved")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (w *Worker) quotaClick(ctx context.Context, expr, message string) error {
	deadline := time.Now().Add(8 * time.Second)
	for {
		raw, _ := w.page.JSString(expr)
		if raw != "" && raw != "null" {
			var pt struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			}
			if err := json.Unmarshal([]byte(raw), &pt); err == nil {
				w.clickPoint(pt.X, pt.Y)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return genError("%s", message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func (w *Worker) clickPoint(x, y float64) {
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		_, _ = w.page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1,
		}, 20*time.Second)
	}
}

// ParseQuota parses English, traditional Chinese and simplified Chinese usage
// text. A plan label alone is not a successful read.
func ParseQuota(text string) *store.Quota {
	normalized := normalizeText(text)
	rawLines := strings.Split(normalized, "\n")
	lines := make([]string, 0, len(rawLines))
	for _, ln := range rawLines {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	start := 0
	for i, ln := range lines {
		lower := strings.ToLower(ln)
		if lower == "usage" || lower == "用量" || lower == "使用量" {
			start = i + 1
			break
		}
	}
	lines = lines[start:]
	end := len(lines)
	for i, ln := range lines {
		lower := strings.ToLower(ln)
		if lower == "language" || lower == "语言" || lower == "語言" {
			end = i
			break
		}
	}
	lines = lines[:end]

	raw := lines
	if len(raw) > 40 {
		raw = raw[:40]
	}
	out := &store.Quota{Raw: strings.Join(raw, "\n")}

	for _, ln := range lines {
		if planRE.MatchString(ln) {
			out.Plan = ln
			break
		}
	}
	joined := strings.Join(lines, "\n")
	if m := weeklyResetEN.FindStringSubmatch(joined); len(m) > 1 {
		out.WeeklyReset = strings.TrimSpace(m[1])
	} else if m := weeklyResetZH.FindStringSubmatch(joined); len(m) > 1 {
		out.WeeklyReset = strings.TrimSpace(m[1])
	}

	extraAt := len(lines)
	for i, ln := range lines {
		if extraMarkerRE.MatchString(ln) {
			extraAt = i
			break
		}
	}

	for _, ln := range lines[:extraAt] {
		if v, ok := percentage(ln); ok {
			out.WeeklyUsedPct = &v
			break
		}
	}
	for _, ln := range lines[extraAt:] {
		if v, ok := percentage(ln); ok {
			out.ExtraUsedPct = &v
			if m := leftParenRE.FindStringSubmatch(ln); len(m) > 1 {
				out.ExtraLeft = strings.TrimSpace(m[1])
			}
			break
		}
	}
	if neverRE.MatchString(joined) {
		out.ExtraExpires = "never"
	}
	out.Found = out.WeeklyUsedPct != nil
	return out
}

func percentage(line string) (float64, bool) {
	m := pctUsedRE.FindStringSubmatch(line)
	if len(m) < 2 {
		m = pctUsedZHRE.FindStringSubmatch(line)
	}
	if len(m) < 2 {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v < 0 || v > 100 {
		return 0, false
	}
	if math.Trunc(v) == v {
		return float64(int(v)), true
	}
	return v, true
}

// normalizeText implements the compatibility normalization this parser needs:
// full-width ASCII, ideographic spaces and non-breaking spaces.
func normalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\u00a0' || r == '\u3000':
			b.WriteRune(' ')
		case r >= '\uff01' && r <= '\uff5e':
			b.WriteRune(r - 0xfee0)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
