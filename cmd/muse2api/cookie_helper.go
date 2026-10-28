package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/cdp"
	"github.com/muse2api/muse2api/internal/config"
)

type browserCookie struct {
	Name     string
	Value    string
	Domain   string
	Expires  float64
	HTTPOnly bool
}

var essentialCookieNames = []string{"hatch_sess", "hatch_vml"}

func runCookieHelper(args []string) int {
	fs := flag.NewFlagSet("cookie-helper", flag.ContinueOnError)
	base := fs.String("base", os.Getenv("MUSE2API_BASE"), "muse2api base URL (for example http://server:18610)")
	key := fs.String("key", os.Getenv("MUSE2API_KEY"), "muse2api API key (starts with m2a_)")
	label := fs.String("label", "", "optional account label")
	timeout := fs.Int("timeout", 300, "seconds to wait for sign-in")
	keepOpen := fs.Bool("keep-open", false, "leave the browser open after a successful import")
	port := fs.Int("port", 9333, "local DevTools port")
	chromeFlag := fs.String("chrome", "", "explicit browser executable path")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	fmt.Println("==========================================================================")
	fmt.Println("  muse2api cookie helper")
	fmt.Println("==========================================================================")
	fmt.Println()
	if *base == "" || *key == "" {
		fmt.Println("Missing --base or --key.")
		fmt.Println("Example:")
		fmt.Println("  muse2api cookie-helper --base https://your-domain --key m2a_xxx")
		fmt.Println()
		fmt.Println("Copy both values from the admin console (Account Pool tab).")
		return 2
	}
	if *label == "" {
		*label = "muse-" + time.Now().Format("0102-1504")
	}

	chrome := *chromeFlag
	if chrome == "" {
		chrome = findBrowser()
	}
	if chrome == "" {
		fmt.Println("No Chrome, Edge, Chromium or Brave installation was found.")
		fmt.Println(`Use --chrome "full path to browser" to specify one manually.`)
		return 3
	}
	fmt.Println("Browser:", chrome)

	profile, err := os.MkdirTemp("", "muse2api-cookie-profile-*")
	if err != nil {
		fmt.Println("Could not create a temporary profile:", err)
		return 3
	}
	defer os.RemoveAll(profile)

	cmdArgs := []string{
		fmt.Sprintf("--remote-debugging-port=%d", *port),
		"--user-data-dir=" + profile,
		"--no-first-run", "--no-default-browser-check",
		"--no-sandbox", "--disable-gpu",
		"--new-window", "https://muse.ai/",
	}
	proc := exec.Command(chrome, cmdArgs...)
	if err := proc.Start(); err != nil {
		fmt.Println("Could not start the browser:", err)
		return 4
	}
	defer func() {
		if *keepOpen {
			return
		}
		killProcessTree(proc)
	}()

	fmt.Println("Opening an isolated browser window (your daily browser is untouched)...")
	wsURL, err := waitDevTools(*port, 30*time.Second)
	if err != nil {
		fmt.Println("The browser debugging port did not start:", err)
		fmt.Println("If your normal browser is running, close it completely and retry.")
		return 4
	}
	cli, err := cdp.Dial(wsURL, 30*time.Second)
	if err != nil {
		fmt.Println("Could not connect to the browser:", err)
		return 4
	}
	defer cli.Close()

	fmt.Println()
	fmt.Println("Please sign in to muse.ai in the window that just opened.")
	fmt.Println("The helper will capture the session automatically once the chat UI is visible.")
	fmt.Println()

	deadline := time.Now().Add(time.Duration(*timeout) * time.Second)
	got := map[string]browserCookie{}
	lastNote := time.Time{}
	for time.Now().Before(deadline) {
		cookies, err := storageCookies(cli)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		got = cookies
		have := 0
		var names []string
		for _, name := range essentialCookieNames {
			if c, ok := got[name]; ok && c.Value != "" {
				have++
				names = append(names, name)
			}
		}
		if have == len(essentialCookieNames) {
			break
		}
		if time.Since(lastNote) > 5*time.Second {
			lastNote = time.Now()
			left := int(time.Until(deadline).Seconds())
			fmt.Printf("  waiting for sign-in... %d/%d core cookies (%s), %ds left\n", have, len(essentialCookieNames), strings.Join(names, ", "), left)
		}
		time.Sleep(2 * time.Second)
	}
	complete := true
	for _, name := range essentialCookieNames {
		if c, ok := got[name]; !ok || c.Value == "" {
			complete = false
		}
	}
	if !complete {
		fmt.Println()
		fmt.Println("Timed out before the sign-in completed. Run the helper again and finish signing in.")
		return 5
	}

	fmt.Println()
	fmt.Println("Session cookies captured:")
	fmt.Println()
	fmt.Printf("  %-30s %-10s %s\n", "cookie", "httpOnly", "expires")
	fmt.Println("  " + strings.Repeat("-", 62))
	for _, name := range essentialCookieNames {
		c := got[name]
		fmt.Printf("  %-30s %-10s %s\n", name, yesNo(c.HTTPOnly), formatExpiry(int64(c.Expires)))
	}
	fmt.Println()
	fmt.Println("Uploading to muse2api...")

	cookies := map[string]string{}
	expires := map[string]int64{}
	for name, c := range got {
		cookies[name] = c.Value
		if c.Expires > 0 {
			expires[name] = int64(c.Expires)
		}
	}
	payload := map[string]any{"label": *label, "cookies": cookies, "expires": expires}
	raw, _ := json.Marshal(payload)
	baseURL := normalizeBase(*base)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/admin/accounts", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+*key)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Upload failed:", err)
		printManualCookie(got)
		return 6
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("Upload failed (HTTP %d): %s\n", resp.StatusCode, strings.TrimSpace(string(body)))
		printManualCookie(got)
		return 6
	}
	var parsed struct {
		Added []struct {
			Label       string `json:"label"`
			ID          string `json:"id"`
			CookieCount int    `json:"cookie_count"`
		} `json:"added"`
		Warning string `json:"warning"`
	}
	_ = json.Unmarshal(body, &parsed)
	fmt.Println()
	fmt.Println("==========================================================================")
	if len(parsed.Added) > 0 {
		a := parsed.Added[0]
		fmt.Printf("Imported successfully. Account label: %s\n", a.Label)
		fmt.Printf("  account ID: %s\n", a.ID)
		fmt.Printf("  cookies:    %d\n", a.CookieCount)
	} else {
		fmt.Println("Imported successfully.")
	}
	if parsed.Warning != "" {
		fmt.Println("  warning:", parsed.Warning)
	}
	fmt.Println()
	fmt.Printf("Open %s/ to see it in the account pool.\n", baseURL)
	fmt.Println("==========================================================================")
	return 0
}

func storageCookies(cli *cdp.Client) (map[string]browserCookie, error) {
	raw, err := cli.Send("Storage.getCookies", nil, 20*time.Second)
	if err != nil {
		return nil, err
	}
	var decoded struct {
		Cookies []struct {
			Name     string  `json:"name"`
			Value    string  `json:"value"`
			Domain   string  `json:"domain"`
			Expires  float64 `json:"expires"`
			HTTPOnly bool    `json:"httpOnly"`
		} `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	out := map[string]browserCookie{}
	for _, c := range decoded.Cookies {
		domain := strings.TrimPrefix(c.Domain, ".")
		if !strings.Contains(domain, "muse.ai") || c.Name == "" {
			continue
		}
		out[c.Name] = browserCookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Expires: c.Expires, HTTPOnly: c.HTTPOnly}
	}
	return out, nil
}

func waitDevTools(port int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	var last error
	for time.Now().Before(deadline) {
		info, err := cdp.HTTPJSON(url, 2*time.Second)
		if err == nil {
			if ws, _ := info["webSocketDebuggerUrl"].(string); ws != "" {
				return ws, nil
			}
		}
		last = err
		time.Sleep(500 * time.Millisecond)
	}
	if last == nil {
		last = fmt.Errorf("no websocket endpoint")
	}
	return "", last
}

func findBrowser() string {
	path := config.DetectChromium(os.Getenv("MUSE2API_CHROMIUM"))
	if _, err := os.Stat(path); err == nil {
		return path
	}
	if resolved, err := exec.LookPath(path); err == nil {
		return resolved
	}
	return ""
}

func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(cmd.Process.Pid)).Run()
	} else {
		_ = cmd.Process.Kill()
	}
}

func normalizeBase(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(strings.ToLower(s), "http://") && !strings.HasPrefix(strings.ToLower(s), "https://") {
		s = "https://" + s
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	return s
}

func formatExpiry(ts int64) string {
	if ts <= 0 {
		return "session"
	}
	return time.Unix(ts, 0).Format("2006-01-02 15:04")
}

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func printManualCookie(cookies map[string]browserCookie) {
	fmt.Println()
	fmt.Println("The cookies were captured but the upload failed. Paste this line into the admin import box:")
	fmt.Println()
	var parts []string
	for name, c := range cookies {
		parts = append(parts, name+"="+c.Value)
	}
	fmt.Println("  " + strings.Join(parts, "; "))
}
