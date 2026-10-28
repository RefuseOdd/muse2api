package engine

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/muse2api/muse2api/internal/config"
)

func engineTestConfig(t *testing.T) *config.Config {
	t.Helper()
	chrome := findBrowserForEngineTest()
	if chrome == "" {
		t.Skip("no Chromium browser available")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	base := t.TempDir()
	cfg := &config.Config{
		BaseDir:        base,
		ProfileRoot:    filepath.Join(base, "profiles"),
		Chromium:       chrome,
		CDPPort:        port,
		HomeDir:        base,
		SiteURL:        "https://muse.ai/",
		LoginWait:      5,
		MaxConcurrency: 1,
		ImageTimeout:   5,
		VideoTimeout:   5,
		ChatTimeout:    5,
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func findBrowserForEngineTest() string {
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

func TestEngineDOMAttachmentsAndReferenceUpload(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser integration test in short mode")
	}
	cfg := engineTestConfig(t)
	eng := New(cfg)
	if err := eng.Start(); err != nil {
		t.Fatalf("engine start: %v", err)
	}
	t.Cleanup(eng.Stop)

	page, err := eng.openPage()
	if err != nil {
		t.Fatalf("open page: %v", err)
	}
	w := &Worker{eng: eng, page: page, ctxID: page.ctxID}
	t.Cleanup(w.ClosePage)

	// Inject a synthetic muse.ai-like DOM. User bubbles and input previews must
	// be excluded; only the generated presentation is a valid result.
	dom := `(function(){
		document.body.innerHTML = '<div class="group/msg"><img id="preview" src="data:image/png;base64,cHJldmlldw=="></div>'
			+ '<div class="hatch-chat-groupable-bubble chat-user-bubble"><img src="data:image/png;base64,dXNlcg=="></div>'
			+ '<div data-testid="hatch-chat-attachment-presentation-image"><img id="out" src="data:image/png;base64,b3V0cHV0"></div>'
			+ '<form id="composer"><div data-testid="hatch-composer-placeholder-overlay"></div>'
			+ '<input type="file" id="f"><div id="thumbs"></div></form>';
		var input = document.querySelector('#composer input[type=file]');
		input.addEventListener('change', function(){
			var b = document.createElement('button');
			b.setAttribute('aria-label', 'Remove attachment');
			document.querySelector('#composer').appendChild(b);
			var img = document.createElement('img');
			document.querySelector('#thumbs').appendChild(img);
		});
		return true;
	})()`
	if _, err := page.JS(dom); err != nil {
		t.Fatalf("inject DOM: %v", err)
	}

	atts := w.Attachments()
	if len(atts) != 1 {
		t.Fatalf("expected one generated attachment, got %#v", atts)
	}
	if atts[0].Src != "data:image/png;base64,b3V0cHV0" {
		t.Fatalf("input previews leaked into attachments: %#v", atts[0])
	}

	if err := w.AttachImages([]string{"data:image/png;base64,eA=="}); err != nil {
		t.Fatalf("attach reference image: %v", err)
	}
	// Attachments must still exclude the composer.
	atts = w.Attachments()
	if len(atts) != 1 {
		t.Fatalf("composer thumbnails must not be treated as results: %#v", atts)
	}

	// A real video attachment is accepted as a video result.
	videoDOM := `(function(){
		document.body.innerHTML = '<div data-testid="hatch-chat-attachment-presentation-video">'
			+ '<video src="https://example.com/clip.mp4" width="640" height="360"></video></div>';
		return true;
	})()`
	if _, err := page.JS(videoDOM); err != nil {
		t.Fatalf("inject video DOM: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	att, err := w.WaitAttachment(ctx, "", 3*time.Second, "video", nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("wait video attachment: %v", err)
	}
	if att == nil || !hasVideoExt(att.VSrc) {
		t.Fatalf("expected a real video attachment, got %#v", att)
	}

	// A video test id with only a poster image must not satisfy a video request.
	coverDOM := `(function(){
		document.body.innerHTML = '<div data-testid="hatch-chat-attachment-presentation-video">'
			+ '<img src="https://example.com/cover.jpg"></div>';
		return true;
	})()`
	if _, err := page.JS(coverDOM); err != nil {
		t.Fatalf("inject cover DOM: %v", err)
	}
	att, err = w.WaitAttachment(ctx, "", 1500*time.Millisecond, "video", nil, 0, 0, nil)
	if err != nil {
		t.Fatalf("wait cover attachment: %v", err)
	}
	if att != nil {
		t.Fatalf("a static cover must not satisfy a video request: %#v", att)
	}
}

func TestEngineSendAndPollChat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser integration test in short mode")
	}
	cfg := engineTestConfig(t)
	eng := New(cfg)
	if err := eng.Start(); err != nil {
		t.Fatalf("engine start: %v", err)
	}
	t.Cleanup(eng.Stop)
	page, err := eng.openPage()
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{eng: eng, page: page, ctxID: page.ctxID}
	t.Cleanup(w.ClosePage)

	dom := `(function(){
		document.body.innerHTML = '<textarea></textarea>'
			+ '<button aria-label="Send" onclick="window.__sent=document.querySelector(\'textarea\').value;document.querySelector(\'textarea\').value=\'\';">Send</button>'
			+ '<div class="hatch-chat-groupable-bubble chat-user-bubble">question</div>'
			+ '<div class="hatch-chat-groupable-bubble hatch-agent-bubble-bg">assistant reply</div>';
		return true;
	})()`
	if _, err := page.JS(dom); err != nil {
		t.Fatal(err)
	}
	sent, err := w.send("hello world")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sent != "clicked" && sent != "enter-sent" {
		t.Fatalf("unexpected send result: %s", sent)
	}
	value, _ := page.JSString(`window.__sent || ''`)
	if value != "hello world" {
		t.Fatalf("prompt was not entered into the composer: %q", value)
	}
	count, text, stop := w.pollChat()
	if count != 1 || text != "assistant reply" || stop {
		t.Fatalf("unexpected chat poll: cnt=%d txt=%q stop=%v", count, text, stop)
	}
	if _, err := page.JS(`document.body.insertAdjacentHTML('beforeend','<button data-testid="hatch-composer-stop-button">Stop</button>'); true`); err != nil {
		t.Fatal(err)
	}
	_, _, stop = w.pollChat()
	if !stop {
		t.Fatalf("stop button was not detected")
	}
}

func TestEngineWaitReadyClassifier(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping browser integration test in short mode")
	}
	cfg := engineTestConfig(t)
	eng := New(cfg)
	if err := eng.Start(); err != nil {
		t.Fatalf("engine start: %v", err)
	}
	t.Cleanup(eng.Stop)
	page, err := eng.openPage()
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{eng: eng, page: page, ctxID: page.ctxID}
	t.Cleanup(w.ClosePage)

	if _, err := page.JS(`document.body.innerHTML = '<textarea></textarea>'; true`); err != nil {
		t.Fatal(err)
	}
	if !w.waitWSReady(context.Background(), 2*time.Second) {
		t.Fatalf("a simple textarea page should be ready")
	}
	if _, err := page.JS(`document.body.innerHTML = '<div>Connecting...</div><textarea></textarea>'; true`); err != nil {
		t.Fatal(err)
	}
	if w.waitWSReady(context.Background(), 400*time.Millisecond) {
		t.Fatalf("Connecting... should not be ready")
	}
}
