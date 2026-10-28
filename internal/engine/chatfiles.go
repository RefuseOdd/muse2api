package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fileHintRE = regexp.MustCompile(`attached|attachment|file|deliverable|artifact|download|shared`)

const installBlobHookJS = `(function(){
  if (!window.__museBlobs) window.__museBlobs = [];
  if (window.__museBlobHooked) return String(window.__museBlobs.length);
  var oc = URL.createObjectURL.bind(URL);
  URL.createObjectURL = function(obj) {
    var u = oc(obj);
    try { window.__museBlobs.push({url: u, size: obj && obj.size, type: obj && obj.type}); } catch (e) {}
    return u;
  };
  window.__museBlobHooked = true;
  return String(window.__museBlobs.length);
})()`

const markBlobsJS = `(function(){
  if (!window.__museBlobs) window.__museBlobs = [];
  window.__museBlobsMark = window.__museBlobs.length;
  return String(window.__museBlobsMark);
})()`

const fileCardsJS = `(function(){
  var out = [];
  var cards = document.querySelectorAll('[data-testid="hatch-chat-attachment-presentation-file"]');
  for (var i = 0; i < cards.length; i++) {
    var c = cards[i];
    if (c.closest('form, [class*=chat-user-bubble], [class*="group/msg"]')) continue;
    var fc = c.querySelector('[data-testid^="hatch-chat-file-card-"]');
    var tid = fc ? (fc.getAttribute('data-testid') || '') : '';
    var hex = tid.replace('hatch-chat-file-card-', '');
    var titleEl = c.querySelector('[title]');
    var title = titleEl ? (titleEl.getAttribute('title') || '') : '';
    var r = (fc || c).getBoundingClientRect();
    if (r.width < 5 || r.height < 5) continue;
    out.push({title: title, hex: hex, x: r.x + r.width/2, y: r.y + r.height/2});
  }
  return JSON.stringify(out);
})()`

const viewerButtonRectJS = `(function(label){
  var b = [...document.querySelectorAll('button')].filter(function(x){
    return x.offsetParent !== null && (x.getAttribute('aria-label')||'') === label;
  })[0];
  if (!b) return 'null';
  var r = b.getBoundingClientRect();
  return JSON.stringify({x: r.x + r.width/2, y: r.y + r.height/2});
})(%s)`

const downloadMenuItemRectJS = `(function(){
  var items = [...document.querySelectorAll('[role="menuitem"]')].filter(function(i){
    return i.offsetParent !== null && /download/i.test(i.innerText||'');
  });
  if (!items.length) return 'null';
  var r = items[0].getBoundingClientRect();
  return JSON.stringify({x: r.x + r.width/2, y: r.y + r.height/2});
})()`

const newBlobsSinceMarkJS = `(function(){
  var all = window.__museBlobs || [];
  var mark = window.__museBlobsMark || 0;
  return JSON.stringify(all.slice(mark));
})()`

type fileCard struct {
	Title string  `json:"title"`
	Hex   string  `json:"hex"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type blobEntry struct {
	URL  string `json:"url"`
	Size int64  `json:"size"`
	Type string `json:"type"`
}

func (w *Worker) installBlobHook() {
	if w.page == nil {
		return
	}
	_, _ = w.page.JSString(installBlobHookJS)
	_, _ = w.page.JSString(markBlobsJS)
}

func (w *Worker) trustedClick(x, y float64) {
	for _, typ := range []string{"mousePressed", "mouseReleased"} {
		_, _ = w.page.Send("Input.dispatchMouseEvent", map[string]any{
			"type": typ, "x": x, "y": y, "button": "left", "clickCount": 1,
		}, 30*time.Second)
	}
}

func (w *Worker) clickVisibleButton(label string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && w.page.context().Err() == nil {
		raw, _ := w.page.JSString(fmt.Sprintf(viewerButtonRectJS, jsonString(label)))
		if raw != "" && raw != "null" {
			var pt struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			}
			if json.Unmarshal([]byte(raw), &pt) == nil {
				w.trustedClick(pt.X, pt.Y)
				return true
			}
		}
		w.pause(300 * time.Millisecond)
	}
	return false
}

func (w *Worker) fileCards() []fileCard {
	raw, _ := w.page.JSString(fileCardsJS)
	if raw == "" {
		return nil
	}
	var cards []fileCard
	if json.Unmarshal([]byte(raw), &cards) != nil {
		return nil
	}
	return cards
}

func hexToName(hexPath string) string {
	if hexPath == "" {
		return ""
	}
	raw, err := hex.DecodeString(hexPath)
	if err != nil {
		return ""
	}
	parts := strings.Split(string(raw), "/")
	return parts[len(parts)-1]
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func chatFileExt(name, mime string) string {
	if name != "" {
		if ext := strings.ToLower(filepath.Ext(name)); ext != "" {
			return ext
		}
	}
	return PickExt(mime, "", "")
}

const lastBubbleTextJS = `(function(){
  var bs = [...document.querySelectorAll('div[class*="hatch-agent-bubble-bg"]')];
  return bs.length ? (bs[bs.length-1].innerText || '') : '';
})()`

func (w *Worker) captureChatFiles(ctx context.Context) []ChatFile {
	if !w.eng.cfg.ChatFiles || w.page == nil {
		return nil
	}
	var cards []fileCard
	deadline := time.Now().Add(3 * time.Second)
	hardDeadline := time.Now().Add(12 * time.Second)
	for {
		if ctx.Err() != nil {
			return nil
		}
		cards = w.fileCards()
		if len(cards) > 0 || time.Now().After(hardDeadline) {
			break
		}
		if time.Now().After(deadline) {
			text, _ := w.page.JSString(lastBubbleTextJS)
			if !fileHintRE.MatchString(strings.ToLower(text)) {
				break
			}
		}
		w.pause(500 * time.Millisecond)
	}
	if len(cards) == 0 {
		return nil
	}
	log.Printf("[chat files] %d file card(s) detected", len(cards))
	if len(cards) > MaxChatFiles {
		cards = cards[:MaxChatFiles]
	}
	if err := ctx.Err(); err != nil {
		return nil
	}
	os.MkdirAll(w.eng.cfg.MediaDir(), 0o755)

	files := make([]ChatFile, 0, len(cards))
	for _, card := range cards {
		if ctx.Err() != nil {
			break
		}
		file := w.captureOneChatFile(ctx, card)
		if file != nil {
			files = append(files, *file)
			log.Printf("[chat files] captured %s (%d bytes) as %s", file.Name, file.Size, file.Filename)
		} else {
			log.Printf("[chat files] failed to capture %q", card.Title)
		}
	}
	return files
}

func (w *Worker) captureOneChatFile(ctx context.Context, card fileCard) *ChatFile {
	name := card.Title
	if name == "" {
		name = hexToName(card.Hex)
	}
	if name == "" {
		name = "muse-file"
	}

	before := map[string]bool{}
	if entries, err := os.ReadDir(w.page.downloadDir); err == nil {
		for _, e := range entries {
			before[e.Name()] = true
		}
	}

	w.trustedClick(card.X, card.Y)
	if !w.clickVisibleButton("More actions") {
		log.Printf("[chat files] viewer did not open for %q", name)
		return nil
	}
	dlDeadline := time.Now().Add(3 * time.Second)
	clickedDownload := false
	for time.Now().Before(dlDeadline) && w.page.context().Err() == nil {
		raw, _ := w.page.JSString(downloadMenuItemRectJS)
		if raw != "" && raw != "null" {
			var pt struct {
				X float64 `json:"x"`
				Y float64 `json:"y"`
			}
			if json.Unmarshal([]byte(raw), &pt) == nil {
				w.trustedClick(pt.X, pt.Y)
				clickedDownload = true
				break
			}
		}
		w.pause(300 * time.Millisecond)
	}
	if !clickedDownload {
		log.Printf("[chat files] download menu item not found for %q", name)
		w.closeViewer()
		return nil
	}

	waitDeadline := time.Now().Add(20 * time.Second)
	var blob *blobEntry
	var downloadedPath string
	for time.Now().Before(waitDeadline) && w.page.context().Err() == nil {
		if ctx.Err() != nil {
			break
		}
		if blob == nil {
			raw, _ := w.page.JSString(newBlobsSinceMarkJS)
			if raw != "" {
				var entries []blobEntry
				if json.Unmarshal([]byte(raw), &entries) == nil {
					for i := range entries {
						if entries[i].Size > 0 {
							blob = &entries[i]
							break
						}
					}
				}
			}
		}
		if entries, err := os.ReadDir(w.page.downloadDir); err == nil {
			for _, e := range entries {
				n := e.Name()
				if before[n] || strings.HasSuffix(n, ".crdownload") {
					continue
				}
				p := filepath.Join(w.page.downloadDir, n)
				if st, err := os.Stat(p); err == nil && st.Size() > 0 {
					downloadedPath = p
				}
			}
		}
		if downloadedPath != "" {
			break
		}
		w.pause(500 * time.Millisecond)
	}
	if downloadedPath == "" && blob == nil {
		log.Printf("[chat files] no blob or download appeared for %q", name)
		w.closeViewer()
		return nil
	}

	var data []byte
	var mime string
	if downloadedPath != "" {
		raw, err := os.ReadFile(downloadedPath)
		if err != nil {
			log.Printf("[chat files] downloaded file unreadable for %q: %v", name, err)
			w.closeViewer()
			return nil
		}
		data = raw
	} else {
		bdata, bmime, _, err := w.extractBytes(ctx, blob.URL, "file")
		if err != nil || len(bdata) == 0 {
			log.Printf("[chat files] blob fetch failed for %q: %v", name, err)
			w.closeViewer()
			return nil
		}
		data = bdata
		mime = bmime
	}
	if len(data) > MaxChatFileBytes {
		log.Printf("[chat files] %q exceeds the 100 MiB limit", name)
		w.closeViewer()
		return nil
	}
	if mime == "" && blob != nil {
		mime = blob.Type
	}
	filename := randomHex(16) + chatFileExt(name, mime)
	dst := filepath.Join(w.eng.cfg.MediaDir(), filename)
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		log.Printf("[chat files] write failed for %q: %v", name, err)
		w.closeViewer()
		return nil
	}
	w.closeViewer()
	return &ChatFile{Name: name, Filename: filename, Size: int64(len(data)), MIME: mime}
}

func (w *Worker) closeViewer() {
	w.clickVisibleButton("Close")
}
