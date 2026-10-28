package cdp

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func findTestChrome() string {
	if v := os.Getenv("MUSE2API_CHROMIUM"); v != "" {
		if st, err := os.Stat(v); err == nil && !st.IsDir() {
			return v
		}
	}
	candidates := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/google-chrome",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, `Google\Chrome\Application\chrome.exe`))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "msedge"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	return port
}

func TestCDPWebSocketAgainstChrome(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser integration test in short mode")
	}
	chrome := findTestChrome()
	if chrome == "" {
		t.Skip("no Chromium browser available")
	}
	port := freePort(t)
	profile := t.TempDir()
	cmd := exec.Command(chrome,
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--disable-dev-shm-usage", "--no-first-run", "--no-default-browser-check",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		"--remote-allow-origins=*",
		"--user-data-dir="+profile,
		"about:blank",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	t.Cleanup(func() {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
		} else {
			_ = cmd.Process.Kill()
		}
	})

	var version map[string]any
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		info, err := HTTPJSON(fmt.Sprintf("http://127.0.0.1:%d/json/version", port), time.Second)
		if err == nil {
			version = info
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if version == nil {
		t.Fatal("DevTools endpoint did not start")
	}
	wsURL, _ := version["webSocketDebuggerUrl"].(string)
	if wsURL == "" {
		t.Fatal("missing browser websocket URL")
	}
	browser, err := Dial(wsURL, 10*time.Second)
	if err != nil {
		t.Fatalf("dial browser: %v", err)
	}
	defer browser.Close()

	raw, err := browser.Send("Target.createTarget", map[string]any{"url": "about:blank"}, 10*time.Second)
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &target); err != nil || target.TargetID == "" {
		t.Fatalf("invalid target response: %s", raw)
	}
	pageURL := fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", port, target.TargetID)
	page, err := Dial(pageURL, 10*time.Second)
	if err != nil {
		t.Fatalf("dial page: %v", err)
	}
	defer page.Close()
	if _, err := page.Send("Runtime.enable", nil, 10*time.Second); err != nil {
		t.Fatalf("runtime enable: %v", err)
	}

	value, err := page.Evaluate(`1+2`, false, 10*time.Second)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if n, ok := value.(float64); !ok || n != 3 {
		t.Fatalf("unexpected evaluate result: %#v", value)
	}

	big, err := page.EvaluateString(`"x".repeat(200000)`, false, 10*time.Second)
	if err != nil {
		t.Fatalf("large frame: %v", err)
	}
	if len(big) != 200000 {
		t.Fatalf("large frame truncated: %d", len(big))
	}

	promise, err := page.EvaluateString(`new Promise(function(resolve){setTimeout(function(){resolve("ok")},30);})`, true, 10*time.Second)
	if err != nil || promise != "ok" {
		t.Fatalf("await promise: %q %v", promise, err)
	}

	if _, err := browser.Send("Target.closeTarget", map[string]any{"targetId": target.TargetID}, 10*time.Second); err != nil {
		t.Fatalf("close target: %v", err)
	}
	if !strings.Contains(wsURL, "/devtools/browser/") {
		t.Fatalf("unexpected browser websocket URL: %s", wsURL)
	}
}
