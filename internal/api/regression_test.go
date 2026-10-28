package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

func TestChatQueueDeadline(t *testing.T) {
	s, _ := newTestServer(t, "")
	w, err := s.eng.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.eng.Release(w)
	parent, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = s.streamChatReal(parent, "test", nil, 30*time.Millisecond, func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 300*time.Millisecond {
		t.Fatalf("chat queue deadline: %v (%v)", err, time.Since(start))
	}
}

func TestClearRejectsInvalidBodies(t *testing.T) {
	s, _ := newTestServer(t, "")
	task, err := s.store.CreateTask("image", "keep", func(t *store.Task) { t.Status = "completed" })
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"keep":50`, `{"keep":"50"}`, `{"keep":-1}`, `{"keep":0.5}`, `null`, `{} {}`} {
		req := httptest.NewRequest("POST", "/admin/tasks/clear", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-secret")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code < 400 || s.store.GetTask(task.ID) == nil {
			t.Fatalf("invalid body %q mutated tasks: %d", body, rec.Code)
		}
	}
}

func TestRenewPreservesImportedCookies(t *testing.T) {
	s, _ := newTestServer(t, "")
	acc, err := s.store.AddAccount(map[string]string{"hatch_sess": "old", "hatch_vml": "old"}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	started, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.renewSessionFn = func(ctx context.Context, c map[string]string, e map[string]int64, w bool) (*engine.SessionRenewResult, error) {
		close(started)
		<-resume
		return &engine.SessionRenewResult{OK: true, Cookies: c, CookiesExp: e}, nil
	}
	go func() { defer close(done); s.keepalive.probeAccountSync(context.Background(), acc.ID, false) }()
	<-started
	rec := doRequest(t, s, "POST", "/admin/accounts/"+acc.ID+"/cookies", map[string]any{"cookies": map[string]string{"hatch_sess": "new", "hatch_vml": "new"}}, true)
	close(resume)
	<-done
	if rec.Code != 200 || s.store.GetAccount(acc.ID).Cookies["hatch_sess"] != "new" {
		t.Fatalf("import overwritten: %d", rec.Code)
	}
}

func TestActiveTasksKeepAdmission(t *testing.T) {
	s, _ := newTestServer(t, "")
	var first *store.Task
	for i := 0; i < 8; i++ {
		task, err := s.store.CreateTask("image", "queued")
		if err != nil {
			t.Fatal(err)
		}
		first = task
	}
	rec := doRequest(t, s, "DELETE", "/admin/tasks/"+first.ID, nil, true)
	if rec.Code != 409 {
		t.Fatalf("active deletion: %d", rec.Code)
	}
	rec = doRequest(t, s, "POST", "/admin/tasks/clear", map[string]any{"keep": 0}, true)
	if rec.Code != 200 || s.store.CountActiveTasks() != 8 {
		t.Fatal("clear removed active jobs")
	}
	for _, path := range []string{"/v1/images/tasks", "/v1/videos"} {
		rec = doRequest(t, s, "POST", path, map[string]any{"prompt": "overflow"}, true)
		if rec.Code != 429 {
			t.Fatalf("queue %s: %d", path, rec.Code)
		}
	}
	rec = doRequest(t, s, "GET", "/v1/videos/"+first.ID, nil, true)
	if rec.Code != 404 {
		t.Fatal("video endpoint returned image task")
	}
}

func TestFailedSavesAreNotAcknowledged(t *testing.T) {
	s, cfg := newTestServer(t, "")
	if err := os.Mkdir(cfg.AccountsFile(), 0700); err != nil {
		t.Fatal(err)
	}
	rec := doRequest(t, s, "POST", "/admin/accounts", map[string]any{"cookies": map[string]string{"hatch_sess": "fake"}}, true)
	if rec.Code != 500 || len(s.store.ListAccounts()) != 0 {
		t.Fatalf("failed import acknowledged: %d", rec.Code)
	}
	cfg.EnvPath = t.TempDir()
	rec = doRequest(t, s, "POST", "/admin/apikey/rotate", nil, true)
	if rec.Code != 500 || s.APIKey() != "test-secret" {
		t.Fatal("failed rotation changed live key")
	}
	cfg.ExternalKey = true
	rec = doRequest(t, s, "POST", "/admin/apikey/rotate", nil, true)
	if rec.Code != 409 {
		t.Fatal("environment-owned key rotated")
	}
}

func TestPushPreservesUnrelatedIndex(t *testing.T) {
	s, cfg := newTestServer(t, "")
	cfg.BaseDir = t.TempDir()
	cfg.RepoURL = "owner/repo"
	git := func(args ...string) string {
		t.Helper()
		out, err := runGit(cfg.BaseDir, nil, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return out
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(cfg.BaseDir, ".env"), []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "-f", ".env")
	before := git("diff", "--cached", "--name-only", "-z")
	if _, err := s.update.Push("test", ""); err == nil {
		t.Fatal("push accepted unrelated staged file")
	}
	if after := git("diff", "--cached", "--name-only", "-z"); after != before {
		t.Fatal("push changed user's index")
	}
}

func TestCheckoutFailureStopsInstall(t *testing.T) {
	remote, local := t.TempDir(), t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := runGit(dir, nil, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return out
	}
	git(remote, "init", "-b", "main")
	git(remote, "config", "user.name", "test")
	git(remote, "config", "user.email", "test@example.test")
	os.WriteFile(filepath.Join(remote, "README.md"), []byte("base"), 0600)
	git(remote, "add", ".")
	git(remote, "commit", "-m", "base")
	git(local, "clone", remote, ".")
	before := strings.TrimSpace(git(local, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(remote, "collision"), []byte("upstream"), 0600)
	git(remote, "add", ".")
	git(remote, "commit", "-m", "next")
	os.WriteFile(filepath.Join(local, "collision"), []byte("local"), 0600)
	if err := installGit(local, "main"); err == nil {
		t.Fatal("failed checkout reported success")
	}
	if got := strings.TrimSpace(git(local, "rev-parse", "HEAD")); got != before {
		t.Fatal("failed checkout advanced HEAD")
	}
	data, _ := os.ReadFile(filepath.Join(local, "collision"))
	if string(data) != "local" {
		t.Fatal("local collision overwritten")
	}
}

func TestConfiguredOrigins(t *testing.T) {
	s, cfg := newTestServer(t, "")
	cfg.CORSOrigins = "https://allowed.test"
	for _, origin := range []string{"https://allowed.test", "https://other.test"} {
		req := httptest.NewRequest("OPTIONS", "/v1/models", nil)
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		got := rec.Header().Get("Access-Control-Allow-Origin")
		if (origin == cfg.CORSOrigins && got != origin) || (origin != cfg.CORSOrigins && got != "") {
			t.Fatalf("origin %s allowed as %s", origin, got)
		}
	}
}

func TestSourcePushKeepsRemoteAncestry(t *testing.T) {
	remote, source := t.TempDir(), t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := runGit(dir, nil, args...)
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	git(remote, "init", "-b", "main")
	git(remote, "config", "user.name", "test")
	git(remote, "config", "user.email", "test@example.test")
	os.WriteFile(filepath.Join(remote, "README.md"), []byte("base"), 0600)
	git(remote, "add", ".")
	git(remote, "commit", "-m", "base")
	base := git(remote, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(source, "README.md"), []byte("local update"), 0600)
	if err := ensureGitRepo(source, remote, "main"); err != nil {
		t.Fatal(err)
	}
	if err := basePush(source, "main"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(source, "README.md"))
	if string(data) != "local update" {
		t.Fatal("bootstrap replaced source files")
	}
	git(source, "add", "README.md")
	git(source, "commit", "-m", "local change")
	if parent := git(source, "rev-parse", "HEAD^"); parent != base {
		t.Fatalf("source commit lost remote ancestry: %s", parent)
	}
	// A disposable bare receiver verifies the push without modifying the seed checkout.
	receiver := t.TempDir()
	git(receiver, "init", "--bare")
	git(source, "push", receiver, "HEAD:main")
}

func TestReloginRenewsBeforeBrowserLoad(t *testing.T) {
	s, _ := newTestServer(t, "")
	acc, err := s.store.AddAccount(map[string]string{"hatch_sess": "old", "hatch_vml": "old"}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	s.renewSessionFn = func(ctx context.Context, c map[string]string, e map[string]int64, wake bool) (*engine.SessionRenewResult, error) {
		called = true
		if !wake {
			t.Error("explicit relogin skipped VM wake")
		}
		return &engine.SessionRenewResult{OK: true, Cookies: map[string]string{"hatch_sess": "renewed", "hatch_vml": "renewed"}, CookiesExp: e}, nil
	}
	// Test server deliberately has no browser executable; renewal must happen before that failure.
	rec := doRequest(t, s, "POST", "/admin/relogin", nil, true)
	if !called || rec.Code != 502 || s.store.GetAccount(acc.ID).Cookies["hatch_sess"] != "renewed" {
		t.Fatalf("relogin skipped renewal: called=%v code=%d", called, rec.Code)
	}
}
