package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/muse2api/muse2api/internal/cdp"
	"github.com/muse2api/muse2api/internal/config"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContextTeardownAllowance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		io.Copy(io.Discard, conn) // Accept disposal requests without replying.
	}))
	defer server.Close()
	cli, err := cdp.Dial(strings.Replace(server.URL, "http://", "ws://", 1), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	eng := New(&config.Config{MaxConcurrency: 1})
	page := &Page{eng: eng, browser: cli, ctxID: "stalled"}
	start := time.Now()
	page.Close()
	elapsed := time.Since(start)
	if elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("teardown allowance: %v", elapsed)
	}
}

func TestBrowserIsolationAndDownloads(t *testing.T) {
	if testing.Short() {
		t.Skip("browser integration")
	}
	cfg := engineTestConfig(t)
	eng := New(cfg)
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Stop)
	first, err := eng.openPage()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := eng.openPage()
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if first.ctxID == "" || first.ctxID == second.ctxID || first.downloadDir == second.downloadDir {
		t.Fatal("pages share context or download directory")
	}
	for i, page := range []*Page{first, second} {
		_, err = page.Send("Network.setCookie", map[string]any{"name": "session", "value": []string{"first", "second"}[i], "url": "https://example.test/"}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := first.Send("Network.getCookies", map[string]any{"urls": []string{"https://example.test/"}}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Cookies []struct {
			Value string `json:"value"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Cookies) != 1 || got.Cookies[0].Value != "first" {
		t.Fatal("account cookies leaked between contexts")
	}
	_, err = first.JS(`document.body.innerHTML='<div data-testid="hatch-chat-attachment-presentation-image"><img src="data:image/png;base64,eA=="><button aria-label="Download">Download</button></div>';true`)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cfg.DownloadDir(), "unrelated.png"), []byte("other task"), 0600)
	os.WriteFile(filepath.Join(second.downloadDir, "other.png"), []byte("second task"), 0600)
	w := &Worker{eng: eng, page: first, ctxID: first.ctxID}
	if path := w.downloadFallback(context.Background(), "data:image/png;base64,eA==", 200*time.Millisecond); path != "" {
		t.Fatalf("selected another task's file: %s", path)
	}
	// A context-creation error must not open a default-context page as fallback.
	eng.browser.Close()
	if page, err := eng.openPage(); err == nil {
		page.Close()
		t.Fatal("isolation failure silently opened shared target")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := eng.StartContext(ctx); err != nil {
		t.Fatalf("browser reconnect: %v", err)
	}
	recovered, err := eng.openContext(ctx)
	if err != nil {
		t.Fatalf("page after reconnect: %v", err)
	}
	recovered.Close()
}

func TestChatWaitsThroughOutputPause(t *testing.T) {
	if testing.Short() {
		t.Skip("browser integration")
	}
	cfg := engineTestConfig(t)
	eng := New(cfg)
	if err := eng.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Stop)
	page, err := eng.openPage()
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()
	w := &Worker{eng: eng, page: page, ctxID: page.ctxID, accID: "test"}
	_, err = page.JS(`document.body.innerHTML='<textarea></textarea><button aria-label="Send">Send</button>';document.querySelector('button').onclick=function(){document.querySelector('textarea').value='';document.body.insertAdjacentHTML('beforeend','<div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg">partial</div><button data-testid="hatch-composer-stop-button">Stop</button>');setTimeout(function(){document.querySelector('.hatch-agent-bubble-bg').textContent='partial complete';document.querySelector('[data-testid="hatch-composer-stop-button"]').remove()},6000)};true`)
	if err != nil {
		t.Fatal(err)
	}
	var text string
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = w.ChatStream(ctx, nil, "test", nil, 9*time.Second, "test", nil, func(chunk string) error { text += chunk; return nil })
	if err != nil || text != "partial complete" {
		t.Fatalf("chat truncated during active pause: %q %v", text, err)
	}
}

func TestReferenceDNSCancellation(t *testing.T) {
	old := refLookupIP
	defer func() { refLookupIP = old }()
	refLookupIP = func(ctx context.Context, host string) ([]net.IP, error) { <-ctx.Done(); return nil, ctx.Err() }
	eng := New(&config.Config{MaxConcurrency: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := eng.NormalizeImageContext(ctx, "https://example.test/image.png")
	if err == nil || time.Since(start) > 300*time.Millisecond {
		t.Fatal("reference DNS ignored cancellation")
	}
}
