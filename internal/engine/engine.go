package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/muse2api/muse2api/internal/cdp"
	"github.com/muse2api/muse2api/internal/config"
)

// Engine owns the Chromium process, the browser-level DevTools connection and a
// fixed set of workers. Each worker owns at most one isolated browser context,
// which mirrors the original per-thread page model while bounding concurrency.
type Engine struct {
	cfg *config.Config

	mu        sync.Mutex
	startGate chan struct{}
	procDone  chan struct{}
	proc      *exec.Cmd
	procLog   *os.File
	browser   *cdp.Client
	stopped   chan struct{}
	stopOnce  sync.Once

	workers []*Worker
	free    chan *Worker
	up      atomic.Bool

	preferredMu sync.Mutex
	preferredID string
}

// Worker is an exclusive browser slot. It is checked out for one operation at
// a time, so its fields never need their own lock.
type Worker struct {
	eng     *Engine
	page    *Page
	ctxID   string
	accID   string
	cookies map[string]string
	expires map[string]int64
}

// Page is one DevTools page target.
type Page struct {
	cli         *cdp.Client
	eng         *Engine
	port        int
	ctxID       string
	browser     *cdp.Client
	downloadDir string
	ctx         context.Context
}

func New(cfg *config.Config) *Engine {
	e := &Engine{
		cfg:       cfg,
		stopped:   make(chan struct{}),
		startGate: make(chan struct{}, 1),
	}
	limit := cfg.MaxConcurrency
	if limit < 1 {
		limit = 1
	}
	e.free = make(chan *Worker, limit)
	for i := 0; i < limit; i++ {
		w := &Worker{eng: e}
		e.workers = append(e.workers, w)
		e.free <- w
	}
	return e
}

func (e *Engine) Config() *config.Config { return e.cfg }

// Acquire checks out a browser slot. It blocks until one is free, the context
// is cancelled, or the engine stops.
func (e *Engine) Acquire(ctx context.Context) (*Worker, error) {
	select {
	case <-e.stopped:
		return nil, genError("browser engine is stopped")
	default:
	}
	select {
	case w := <-e.free:
		return w, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.stopped:
		return nil, genError("browser engine is stopped")
	}
}

func (e *Engine) Release(w *Worker) {
	if w == nil {
		return
	}
	select {
	case <-e.stopped:
		w.ClosePage()
		return
	default:
	}
	select {
	case e.free <- w:
	default:
		// Should not happen; never leak a worker.
		w.ClosePage()
	}
}

// TryAcquire is a non-blocking variant used by maintenance tasks.
func (e *Engine) TryAcquire() (*Worker, bool) {
	select {
	case w := <-e.free:
		return w, true
	default:
		return nil, false
	}
}

func (e *Engine) PreferredAccount() string {
	e.preferredMu.Lock()
	defer e.preferredMu.Unlock()
	return e.preferredID
}

func (e *Engine) SetPreferredAccount(id string) {
	e.preferredMu.Lock()
	e.preferredID = id
	e.preferredMu.Unlock()
}

func (e *Engine) BrowserRunning() bool { return e.up.Load() }

func (e *Engine) debugURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d/json/version", e.cfg.CDPPort)
}

// Start launches Chromium if needed and connects to its browser endpoint.
func (e *Engine) Start() error { return e.StartContext(context.Background()) }
func (e *Engine) StartContext(ctx context.Context) error {
	select {
	case e.startGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.stopped:
		return genError("browser engine is stopped")
	}
	defer func() { <-e.startGate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.browser != nil {
		if !e.browser.Closed() && (e.proc == nil || e.processAlive()) {
			e.up.Store(true)
			return nil
		}
		e.browser.Close()
		e.browser = nil
		e.up.Store(false)
	}
	select {
	case <-e.stopped:
		return genError("browser engine is stopped")
	default:
	}

	// Reuse an already healthy debug endpoint before spawning another process.
	if e.proc == nil || e.processAlive() {
		if info, err := cdp.HTTPContext(ctx, http.MethodGet, e.debugURL(), time.Second); err == nil {
			if ws, _ := info["webSocketDebuggerUrl"].(string); ws != "" {
				if cli, err := cdp.Dial(ws, remaining(ctx, 30*time.Second)); err == nil {
					e.browser = cli
					e.up.Store(true)
					return nil
				}
			}
		}
	}

	if e.processAlive() {
		return genError("could not reconnect to the running browser")
	}
	for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
		p := filepath.Join(e.cfg.ProfileDir(), name)
		if _, err := os.Lstat(p); err == nil {
			_ = os.Remove(p)
		}
	}
	if err := os.MkdirAll(e.cfg.DataDir(), 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(e.cfg.DataDir(), "chromium.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open chromium log: %w", err)
	}

	args := []string{
		"--headless=new", "--no-sandbox", "--disable-gpu",
		"--disable-dev-shm-usage", "--disable-background-networking",
		"--no-first-run", "--no-default-browser-check",
		"--autoplay-policy=no-user-gesture-required",
		"--window-size=1440,2400",
		fmt.Sprintf("--remote-debugging-port=%d", e.cfg.CDPPort),
		"--remote-allow-origins=*",
		"--user-data-dir=" + e.cfg.ProfileDir(),
		"about:blank",
	}
	cmd := exec.Command(e.cfg.Chromium, args...)
	env := os.Environ()
	if os.Getenv("HOME") == "" && e.cfg.HomeDir != "" {
		env = append(env, "HOME="+e.cfg.HomeDir)
	}
	if e.cfg.ExtraPath != "" {
		old := ""
		for _, item := range env {
			if strings.HasPrefix(strings.ToUpper(item), "PATH=") {
				old = item[len("PATH="):]
				break
			}
		}
		env = append(env, "PATH="+e.cfg.ExtraPath+string(os.PathListSeparator)+old)
	}
	cmd.Env = env
	if st, err := os.Stat(e.cfg.HomeDir); err == nil && st.IsDir() {
		cmd.Dir = e.cfg.HomeDir
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return genError("failed to start Chromium (%s): %v", e.cfg.Chromium, err)
	}
	e.proc = cmd
	e.procLog = logFile
	done := make(chan struct{})
	e.procDone = done
	go func() {
		_ = cmd.Wait()
		close(done)
		e.up.Store(false)
	}()

	var lastErr error
	for i := 0; i < 90; i++ {
		info, err := cdp.HTTPContext(ctx, http.MethodGet, e.debugURL(), 2*time.Second)
		if err == nil {
			ws, _ := info["webSocketDebuggerUrl"].(string)
			if ws != "" {
				cli, derr := cdp.Dial(ws, remaining(ctx, 30*time.Second))
				if derr == nil {
					e.browser = cli
					e.up.Store(true)
					return nil
				}
				lastErr = derr
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.stopped:
			return genError("browser engine is stopped")
		case <-time.After(time.Second):
		}
	}
	return genError("Chromium failed to start: %v", lastErr)
}

func (e *Engine) processAlive() bool {
	if e.proc == nil || e.proc.Process == nil {
		return false
	}
	select {
	case <-e.procDone:
		return false
	default:
		return true
	}
}

// Stop closes pages and the browser, then terminates the owned process.
func (e *Engine) Stop() {
	e.stopOnce.Do(func() {
		close(e.stopped)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.browser != nil {
			e.browser.Close()
			e.browser = nil
		}
		e.up.Store(false)
		if e.proc != nil && e.proc.Process != nil && e.processAlive() {
			if runtime.GOOS == "windows" {
				kill := exec.Command("taskkill", "/F", "/T", "/PID", fmt.Sprint(e.proc.Process.Pid))
				_ = kill.Run()
			} else {
				_ = e.proc.Process.Kill()
			}
		}
		e.proc = nil
		if e.procLog != nil {
			_ = e.procLog.Close()
			e.procLog = nil
		}
	})
}

// ---------- Pages ----------

func (e *Engine) openPage() (*Page, error) {
	return e.openContext(context.Background())
}
func (e *Engine) openContext(ctx context.Context) (*Page, error) {
	e.mu.Lock()
	browser := e.browser
	e.mu.Unlock()
	if browser == nil {
		return nil, genError("browser is not running")
	}
	raw, err := browser.SendContext(ctx, "Target.createBrowserContext", map[string]any{"disposeOnDetach": true}, 30*time.Second)
	if err != nil {
		return nil, genError("failed to create isolated browser context: %v", err)
	}
	var created struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.BrowserContextID == "" {
		return nil, genError("browser did not return an isolated context")
	}
	page := &Page{browser: browser, eng: e, port: e.cfg.CDPPort, ctxID: created.BrowserContextID, ctx: ctx}
	ready := false
	defer func() {
		if !ready {
			page.Close()
		}
	}()
	page.downloadDir, err = os.MkdirTemp(e.cfg.DownloadDir(), "task-")
	if err != nil {
		return nil, err
	}
	raw, err = browser.SendContext(ctx, "Target.createTarget", map[string]any{"url": "about:blank", "browserContextId": page.ctxID}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &target); err != nil || target.TargetID == "" {
		return nil, genError("browser did not return a page target")
	}
	page.cli, err = cdp.Dial(fmt.Sprintf("ws://127.0.0.1:%d/devtools/page/%s", e.cfg.CDPPort, target.TargetID), remaining(ctx, 30*time.Second))
	if err != nil {
		return nil, err
	}
	for _, method := range []string{"Network.enable", "Page.enable", "Runtime.enable"} {
		if _, err := page.Send(method, nil, 30*time.Second); err != nil {
			return nil, err
		}
	}
	if _, err := browser.SendContext(ctx, "Browser.setDownloadBehavior", map[string]any{"behavior": "allow", "downloadPath": page.downloadDir, "browserContextId": page.ctxID}, 30*time.Second); err != nil {
		return nil, err
	}
	ready = true
	return page, nil
}
func remaining(ctx context.Context, limit time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		if left := time.Until(deadline); left < limit {
			if left <= 0 {
				return time.Nanosecond
			}
			return left
		}
	}
	return limit
}

func (w *Worker) Page() *Page                                   { return w.page }
func (w *Worker) Source() (map[string]string, map[string]int64) { return w.cookies, w.expires }

func (w *Worker) ClosePage() {
	if w == nil {
		return
	}
	if w.page != nil {
		w.page.Close()
		w.page = nil
	}
	w.ctxID = ""
	w.accID = ""
}

func (p *Page) Close() {
	if p == nil {
		return
	}
	if p.cli != nil {
		p.cli.Close()
		p.cli = nil
	}
	if p.ctxID != "" && p.eng != nil {
		// Keep the owning connection even if the engine starts another browser.
		if browser := p.browser; browser != nil {
			_, _ = browser.Send("Target.disposeBrowserContext", map[string]any{"browserContextId": p.ctxID}, 2*time.Second)
		}
		p.ctxID = ""
	}
	if p.downloadDir != "" {
		root, err := filepath.Abs(p.eng.cfg.DownloadDir())
		target, targetErr := filepath.Abs(p.downloadDir)
		rel, relErr := filepath.Rel(root, target)
		if err == nil && targetErr == nil && relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			_ = os.RemoveAll(target)
		}
		p.downloadDir = ""
	}
}
func (p *Page) context() context.Context {
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx
}

func (p *Page) ContextID() string { return p.ctxID }

// EnsurePage opens an isolated page using credentials renewed by the API layer.
func (w *Worker) EnsurePage(ctx context.Context, cookies map[string]string, expires map[string]int64, accountID string) (*Page, error) {
	if w.page != nil {
		w.page.ctx = ctx
	}
	if w.page != nil && (accountID == "" || w.accID == accountID) {
		if ready, _ := w.page.JSString("!!document.querySelector('textarea')"); ready == "true" {
			if w.waitWSReady(ctx, 8*time.Second) {
				return w.page, nil
			}
		}
	}
	w.ClosePage()

	page, err := w.eng.openContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := w.applyCookies(page, cookies, expires); err != nil {
		page.Close()
		return nil, err
	}
	if _, err := page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 30*time.Second); err != nil {
		page.Close()
		return nil, genError("failed to open muse.ai: %v", err)
	}
	// Assign before waiting so cleanup owns pages that never become ready.
	w.page = page
	w.cookies = maps.Clone(cookies)
	w.expires = maps.Clone(expires)
	w.ctxID = page.ctxID
	wait := time.Duration(w.eng.cfg.LoginWait) * time.Second
	if w.waitWSReady(ctx, wait) {
		w.accID = accountID
		return page, nil
	}
	body, _ := page.JSString("document.body ? document.body.innerText.slice(0,1200) : ''")
	lower := strings.ToLower(body)
	w.ClosePage()
	if strings.Contains(lower, "connecting...") {
		return nil, genError("page gateway connection timed out (Connecting...), rotating account")
	}
	for _, marker := range []string{"log in", "sign in", "create an account", "登录", "use another account"} {
		if strings.Contains(lower, marker) {
			return nil, authError("muse.ai signed this session out (another login may have replaced it); import the cookie again with the browser extension")
		}
	}
	return nil, genError("muse.ai page load timed out before the chat input appeared; check the server network and retry (session expiry was not confirmed)")
}

func (w *Worker) Refresh(ctx context.Context, cookies map[string]string, expires map[string]int64) (*Page, error) {
	w.ClosePage()
	return w.EnsurePage(ctx, cookies, expires, "")
}

func (w *Worker) applyCookies(page *Page, cookies map[string]string, expires map[string]int64) error {
	now := time.Now()
	for name, value := range cookies {
		if value == "" {
			continue
		}
		exp := float64(now.Add(7 * 24 * time.Hour).Unix())
		if v, ok := expires[name]; ok && v > now.Add(time.Hour).Unix() {
			exp = float64(v)
		}
		for _, domain := range []string{".muse.ai", "muse.ai"} {
			_, _ = page.Send("Network.setCookie", map[string]any{
				"name":    name,
				"value":   value,
				"domain":  domain,
				"path":    "/",
				"secure":  true,
				"expires": exp,
			}, 20*time.Second)
		}
	}
	return nil
}

// ReadCookies returns live cookies including HttpOnly entries.
func (w *Worker) ReadCookies(ctx context.Context) map[string]CookieInfo {
	out := map[string]CookieInfo{}
	if w.page == nil {
		return out
	}
	raw, err := w.page.Send("Network.getCookies", map[string]any{"urls": []string{w.eng.cfg.SiteURL}}, 20*time.Second)
	if err != nil {
		return out
	}
	var decoded struct {
		Cookies []struct {
			Name    string  `json:"name"`
			Value   string  `json:"value"`
			Expires float64 `json:"expires"`
		} `json:"cookies"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return out
	}
	for _, c := range decoded.Cookies {
		if c.Name == "" {
			continue
		}
		out[c.Name] = CookieInfo{Value: c.Value, Expires: int64(c.Expires)}
	}
	return out
}

type CookieInfo struct {
	Value   string
	Expires int64
}

func (w *Worker) waitWSReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		state, err := w.page.JSString(waitReadyJS)
		if err == nil && state == "ready" {
			return true
		}
		w.pause(100 * time.Millisecond)
	}
	return false
}

// ResetThread closes leftover dialogs and ensures a clean thread. For chat, a
// healthy page with few bubbles may be reused to keep first-token latency low.
func (w *Worker) ResetThread(ctx context.Context, forChat bool) {
	if w.page == nil {
		return
	}
	expr := fmt.Sprintf(resetThreadJS, boolJS(forChat))
	needsNav, _ := w.page.JSString(expr)
	if needsNav != "true" {
		return
	}
	_, _ = w.page.Send("Page.navigate", map[string]any{"url": "https://muse.ai/thread/new"}, 30*time.Second)
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		w.pause(80 * time.Millisecond)
		ready, _ := w.page.JSString(`(function(){return document.readyState === 'complete' && !!document.querySelector('textarea') && document.querySelectorAll('div[class*="hatch-chat-groupable-bubble"]').length === 0;})()`)
		if ready == "true" {
			break
		}
	}
	w.waitWSReady(ctx, 12*time.Second)
}

func (w *Worker) ScrollBottom() {
	if w.page == nil {
		return
	}
	_, _ = w.page.JSString(scrollBottomJS)
}

// ---------- Session renewal ----------

// Session endpoints are variables so the state machine can be tested offline.
var (
	museSessionURL = "https://muse.ai/api/session"
	museWakeURL    = "https://muse.ai/api/hatch/vm/wake"
)

func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (e *Engine) RenewSessionHTTP(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*SessionRenewResult, error) {
	curCookies := map[string]string{}
	for k, v := range cookies {
		curCookies[k] = v
	}
	curExp := map[string]int64{}
	for k, v := range expires {
		curExp[k] = v
	}
	headers := map[string]string{
		"User-Agent":     "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36",
		"Origin":         "https://muse.ai",
		"Referer":        "https://muse.ai/thread/new",
		"Sec-Fetch-Site": "same-origin",
		"Sec-Fetch-Mode": "cors",
		"Sec-Fetch-Dest": "empty",
	}
	doGet := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, museSessionURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Set("Cookie", cookieHeader(curCookies))
		return noRedirectClient(12 * time.Second).Do(req)
	}

	resp, err := doGet()
	if err != nil {
		return nil, genError("/api/session network request failed (%T); check the server network/proxy and retry", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, authError("session authentication failed (/api/session HTTP 401); confirm you are signed in on muse.ai and import the cookie again")
	}
	if resp.StatusCode != 200 {
		if resp.StatusCode == 403 {
			return nil, genError("/api/session HTTP 403: access denied; check the server egress/region/access rules. This does not prove the cookie expired")
		}
		return nil, genError("/api/session HTTP %d: upstream request was not successful; retry later and check server networking", resp.StatusCode)
	}
	var session map[string]any
	if err := json.Unmarshal(body, &session); err != nil || session == nil {
		return nil, genError("/api/session returned HTTP 200 with non-JSON data; session state was not confirmed")
	}
	if status, _ := session["status"].(string); status != "assigned" {
		return nil, genError("/api/session returned HTTP 200 without an assigned session; check the account/workspace on muse.ai")
	}
	mergeResponseCookies(resp, curCookies, curExp)

	vmID := asString(session["vm_id"])
	vmState := asString(session["vm_state"])
	wakeOK := false
	wakeRequested := false
	if wakeVM && vmID != "" && vmState != "DISABLED" {
		payload, _ := json.Marshal(map[string]any{
			"vm_id":              vmID,
			"retry_count":        0,
			"connect_attempt_id": uuidV4(),
		})
		req, rerr := http.NewRequestWithContext(ctx, http.MethodPost, museWakeURL, strings.NewReader(string(payload)))
		if rerr == nil {
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", cookieHeader(curCookies))
			wresp, werr := noRedirectClient(8 * time.Second).Do(req)
			if werr == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(wresp.Body, 1<<20))
				_ = wresp.Body.Close()
				wakeRequested = wresp.StatusCode == 200
			}
			if wakeRequested {
				verify, verr := doGet()
				if verr == nil {
					vbody, _ := io.ReadAll(io.LimitReader(verify.Body, 1<<20))
					_ = verify.Body.Close()
					if verify.StatusCode == 200 {
						var vs map[string]any
						if json.Unmarshal(vbody, &vs) == nil && asString(vs["status"]) == "assigned" {
							if s := asString(vs["vm_state"]); s != "" {
								vmState = s
							}
							wakeOK = vmState == "RUNNING"
						}
					}
				}
			}
		}
	}
	return &SessionRenewResult{
		OK:            true,
		Status:        asString(session["status"]),
		VMID:          vmID,
		VMState:       vmState,
		WakeOK:        wakeOK,
		WakeRequested: wakeRequested,
		Cookies:       curCookies,
		CookiesExp:    curExp,
	}, nil
}

func mergeResponseCookies(resp *http.Response, cookies map[string]string, expires map[string]int64) {
	now := time.Now().Unix()
	for _, c := range resp.Cookies() {
		if c.Name == "" {
			continue
		}
		if c.Value != "" {
			cookies[c.Name] = c.Value
		}
		if c.MaxAge > 0 {
			expires[c.Name] = now + int64(c.MaxAge)
		} else if !c.Expires.IsZero() {
			if v := c.Expires.Unix(); v > 0 {
				expires[c.Name] = v
			}
		}
	}
}

func cookieHeader(cookies map[string]string) string {
	parts := make([]string, 0, len(cookies))
	for k, v := range cookies {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

func uuidV4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexs := hex.EncodeToString(b)
	return hexs[0:8] + "-" + hexs[8:12] + "-" + hexs[12:16] + "-" + hexs[16:20] + "-" + hexs[20:32]
}

func boolJS(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

// JS executes a JavaScript expression and returns its by-value result.
func (p *Page) JS(expr string) (any, error) {
	if p == nil || p.cli == nil {
		return nil, errors.New("page is closed")
	}
	return p.cli.EvaluateContext(p.context(), expr, false, 90*time.Second)
}

func (p *Page) JSString(expr string) (string, error) {
	if p == nil || p.cli == nil {
		return "", errors.New("page is closed")
	}
	return p.cli.EvaluateStringContext(p.context(), expr, false, 90*time.Second)
}

func (p *Page) JSAwait(expr string, timeout time.Duration) (string, error) {
	if p == nil || p.cli == nil {
		return "", errors.New("page is closed")
	}
	return p.cli.EvaluateStringContext(p.context(), expr, true, timeout)
}

func (p *Page) Send(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	if p == nil || p.cli == nil {
		return nil, errors.New("page is closed")
	}
	return p.cli.SendContext(p.context(), method, params, timeout)
}

func (w *Worker) pause(d time.Duration) {
	ctx := context.Background()
	if w.page != nil {
		ctx = w.page.context()
	}
	pause(ctx, d)
}

func pause(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
