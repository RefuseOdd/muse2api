package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/muse2api/muse2api/internal/config"
	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

func newTestServer(t *testing.T, publicURL string) (*Server, *config.Config) {
	t.Helper()
	base := t.TempDir()
	cfg := &config.Config{
		BaseDir:        base,
		ProfileRoot:    filepath.Join(base, "profiles"),
		Host:           "127.0.0.1",
		Port:           0,
		PublicURL:      publicURL,
		APIKey:         "test-secret",
		MaxConcurrency: 1,
		ImageTimeout:   5,
		VideoTimeout:   5,
		ChatTimeout:    5,
		LoginWait:      0,
		SiteURL:        "https://muse.ai/",
		Chromium:       "none",
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, _ := store.New(cfg.AccountsFile(), cfg.TasksFile())
	eng := engine.New(cfg)
	s := NewServer(cfg, st, eng)
	t.Cleanup(eng.Stop)
	return s, cfg
}

func doRequest(t *testing.T, s *Server, method, path string, body any, authenticated bool) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		req.Header.Set("Authorization", "Bearer test-secret")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("invalid JSON response (%d): %s: %v", rec.Code, rec.Body.String(), err)
	}
}

func TestAuthAndErrorEnvelopes(t *testing.T) {
	s, _ := newTestServer(t, "")

	rec := doRequest(t, s, http.MethodGet, "/admin/status", nil, false)
	if rec.Code != 401 {
		t.Fatalf("admin without key: %d", rec.Code)
	}
	var adminErr map[string]any
	decodeJSON(t, rec, &adminErr)
	if _, ok := adminErr["detail"]; !ok {
		t.Fatalf("admin error must use detail: %s", rec.Body.String())
	}

	rec = doRequest(t, s, http.MethodGet, "/v1/models", nil, false)
	if rec.Code != 401 {
		t.Fatalf("v1 without key: %d", rec.Code)
	}
	var openaiErr struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	decodeJSON(t, rec, &openaiErr)
	if openaiErr.Error.Message == "" || openaiErr.Error.Code != 401 {
		t.Fatalf("bad OpenAI error envelope: %s", rec.Body.String())
	}

	rec = doRequest(t, s, http.MethodGet, "/healthz", nil, false)
	if rec.Code != 200 {
		t.Fatalf("healthz must be public: %d", rec.Code)
	}

	rec = doRequest(t, s, http.MethodPost, "/v1/images/generations", map[string]any{}, true)
	if rec.Code != 422 {
		t.Fatalf("missing prompt: got %d body=%s", rec.Code, rec.Body.String())
	}
	decodeJSON(t, rec, &openaiErr)
	if !strings.Contains(openaiErr.Error.Message, "prompt is required") {
		t.Fatalf("unexpected validation message: %+v", openaiErr)
	}
}

func TestAdminBaseURLConsistency(t *testing.T) {
	for _, public := range []string{"", "https://api.example.com/"} {
		s, cfg := newTestServer(t, strings.TrimRight(public, "/"))
		rec := doRequest(t, s, http.MethodGet, "/admin/status", nil, true)
		if rec.Code != 200 {
			t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
		}
		var status struct {
			BaseURL string `json:"base_url"`
			Config  struct {
				PublicBase string `json:"public_base"`
			} `json:"config"`
		}
		decodeJSON(t, rec, &status)
		rec = doRequest(t, s, http.MethodGet, "/admin/apikey", nil, true)
		var keyResp struct {
			BaseURL string `json:"base_url"`
		}
		decodeJSON(t, rec, &keyResp)
		if status.BaseURL != keyResp.BaseURL {
			t.Fatalf("base_url mismatch: %q vs %q", status.BaseURL, keyResp.BaseURL)
		}
		if cfg.PublicURL == "" {
			if status.BaseURL != "" {
				t.Fatalf("empty public base must produce an empty base_url")
			}
		} else if status.BaseURL != "https://api.example.com/v1" {
			t.Fatalf("unexpected base_url %q", status.BaseURL)
		}
		if status.Config.PublicBase != cfg.PublicURL {
			t.Fatalf("public_base echo mismatch: %q vs %q", status.Config.PublicBase, cfg.PublicURL)
		}
	}
}

func TestAccountImportAndEssentialWarning(t *testing.T) {
	s, _ := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodPost, "/admin/accounts", map[string]any{
		"label":         "one",
		"cookie_header": "hatch_sess=s; hatch_vml=v; foo=bar",
	}, true)
	if rec.Code != 200 {
		t.Fatalf("import: %d %s", rec.Code, rec.Body.String())
	}
	var imported struct {
		Count            int      `json:"count"`
		EssentialMissing []string `json:"essential_missing"`
		Warning          string   `json:"warning"`
	}
	decodeJSON(t, rec, &imported)
	if imported.Count != 1 || len(imported.EssentialMissing) != 0 || imported.Warning != "" {
		t.Fatalf("unexpected import result: %+v", imported)
	}

	rec = doRequest(t, s, http.MethodPost, "/admin/accounts", map[string]any{
		"batch": "a | hatch_sess=1\nb | foo=2",
	}, true)
	decodeJSON(t, rec, &imported)
	if imported.Count != 2 || len(imported.EssentialMissing) != 1 || imported.EssentialMissing[0] != "hatch_vml" {
		t.Fatalf("expected missing hatch_vml: %+v", imported)
	}
	if imported.Warning == "" {
		t.Fatalf("expected a warning")
	}

	rec = doRequest(t, s, http.MethodGet, "/admin/accounts", nil, true)
	var list struct {
		Accounts []map[string]any `json:"accounts"`
	}
	decodeJSON(t, rec, &list)
	if len(list.Accounts) != 3 {
		t.Fatalf("expected three accounts, got %d", len(list.Accounts))
	}
	if _, leaked := list.Accounts[0]["cookies"]; leaked {
		t.Fatalf("account list must not expose cookies")
	}
}

func TestImageTaskIdempotencyAndAsyncAck(t *testing.T) {
	s, _ := newTestServer(t, "")
	var calls int32
	s.runGenerationFn = func(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
		atomic.AddInt32(&calls, 1)
		return &engine.GenResult{Filename: "fixture.png", Size: 123, Kind: "image"}, "fixture-account", nil
	}

	start := time.Now()
	rec := doRequest(t, s, http.MethodPost, "/v1/images/tasks", map[string]any{
		"prompt": "fixture", "size": "1:1", "timeout": 30,
	}, true)
	elapsed := time.Since(start)
	if rec.Code != 202 {
		t.Fatalf("create task: %d %s", rec.Code, rec.Body.String())
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("async acknowledgement took %s", elapsed)
	}
	var task struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	decodeJSON(t, rec, &task)
	if task.ID == "" {
		t.Fatalf("missing task id")
	}

	deadline := time.Now().Add(3 * time.Second)
	status := ""
	for time.Now().Before(deadline) {
		rec = doRequest(t, s, http.MethodGet, "/v1/images/tasks/"+task.ID, nil, true)
		var polled struct {
			Status string `json:"status"`
		}
		decodeJSON(t, rec, &polled)
		status = polled.Status
		if status == "completed" || status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != "completed" {
		t.Fatalf("task did not complete, status=%s", status)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("generation calls = %d", calls)
	}

	// Same key and request must reuse the original task.
	reqBody := map[string]any{"prompt": "fixture", "size": "1:1", "timeout": 30}
	idemReq := func(body map[string]any, key string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/v1/images/tasks", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-secret")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	first := idemReq(reqBody, "stable-key")
	var firstTask struct {
		ID string `json:"id"`
	}
	decodeJSON(t, first, &firstTask)
	second := idemReq(reqBody, "stable-key")
	if second.Code != 202 {
		t.Fatalf("idempotent replay: %d %s", second.Code, second.Body.String())
	}
	var secondTask struct {
		ID string `json:"id"`
	}
	decodeJSON(t, second, &secondTask)
	if firstTask.ID == "" || firstTask.ID != secondTask.ID {
		t.Fatalf("idempotent replay returned a different id: %q vs %q", firstTask.ID, secondTask.ID)
	}
	conflict := idemReq(map[string]any{"prompt": "different", "size": "1:1", "timeout": 30}, "stable-key")
	if conflict.Code != 409 {
		t.Fatalf("expected 409 for a different request, got %d", conflict.Code)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("unexpected extra generation calls: %d", calls)
	}
}

func TestImageQueueAdmissionCap(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.runGenerationFn = func(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
		return &engine.GenResult{Filename: "x.png", Size: 1, Kind: "image"}, "a", nil
	}
	for i := 0; i < 8; i++ {
		s.store.CreateTask("image", "queued")
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/images/tasks", map[string]any{"prompt": "overflow"}, true)
	if rec.Code != 429 {
		t.Fatalf("expected 429, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestImageEditsReferenceOrder(t *testing.T) {
	s, _ := newTestServer(t, "")
	var gotRefs []string
	s.runGenerationFn = func(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
		gotRefs = append([]string{}, referenceImages...)
		return &engine.GenResult{Filename: "edit.png", Size: 3, Kind: "image"}, "acc", nil
	}
	rec := doRequest(t, s, http.MethodPost, "/v1/images/edits", map[string]any{
		"prompt": "combine",
		"images": []any{
			map[string]any{"url": "data:image/png;base64,eA=="},
			map[string]any{"image_url": "data:image/png;base64,eQ=="},
		},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("edits: %d %s", rec.Code, rec.Body.String())
	}
	if len(gotRefs) != 2 || gotRefs[0] != "data:image/png;base64,eA==" || gotRefs[1] != "data:image/png;base64,eQ==" {
		t.Fatalf("reference order not preserved: %#v", gotRefs)
	}

	conflict := doRequest(t, s, http.MethodPost, "/v1/images/edits", map[string]any{
		"prompt": "x", "images": []any{"data:image/png;base64,eA=="}, "reference_image": "old",
	}, true)
	if conflict.Code != 400 {
		t.Fatalf("expected conflict 400, got %d", conflict.Code)
	}

	// Legacy-only edits still need to reach generation as a reference.
	gotRefs = nil
	legacy := doRequest(t, s, http.MethodPost, "/v1/images/edits", map[string]any{
		"prompt":          "legacy edit",
		"reference_image": "data:image/png;base64,bGVnYWN5",
	}, true)
	if legacy.Code != 200 {
		t.Fatalf("legacy edits: %d %s", legacy.Code, legacy.Body.String())
	}
	if len(gotRefs) != 1 || gotRefs[0] != "data:image/png;base64,bGVnYWN5" {
		t.Fatalf("legacy reference was lost: %#v", gotRefs)
	}
}

func TestMediaEndpoints(t *testing.T) {
	s, cfg := newTestServer(t, "")
	for _, name := range []string{"sample1.webp", "sample2.mp4"} {
		if err := os.WriteFile(filepath.Join(cfg.MediaDir(), name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rec := doRequest(t, s, http.MethodGet, "/admin/media", nil, true)
	var list struct {
		Media []map[string]any `json:"media"`
	}
	decodeJSON(t, rec, &list)
	if len(list.Media) != 2 {
		t.Fatalf("expected two media files, got %d", len(list.Media))
	}

	rec = doRequest(t, s, http.MethodPost, "/admin/media/delete", map[string]any{
		"names": []string{"../app.py", "..\\app.py"},
	}, true)
	if rec.Code != 200 {
		t.Fatalf("bulk delete status: %d", rec.Code)
	}
	var bulk struct {
		Removed int `json:"removed"`
	}
	decodeJSON(t, rec, &bulk)
	if bulk.Removed != 0 {
		t.Fatalf("traversal names must not delete anything")
	}

	rec = doRequest(t, s, http.MethodPost, "/admin/media/delete", map[string]any{"names": []string{"sample1.webp"}}, true)
	decodeJSON(t, rec, &bulk)
	if bulk.Removed != 1 {
		t.Fatalf("expected one removal")
	}
	if _, err := os.Stat(filepath.Join(cfg.MediaDir(), "sample1.webp")); !os.IsNotExist(err) {
		t.Fatalf("file should be gone")
	}

	rec = doRequest(t, s, http.MethodDelete, "/admin/media/sample2.mp4", nil, true)
	if rec.Code != 200 {
		t.Fatalf("single delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, s, http.MethodDelete, "/admin/media/missing.webp", nil, true)
	if rec.Code != 404 {
		t.Fatalf("missing media should be 404, got %d", rec.Code)
	}
}

func TestExtensionZipContainsAllFiles(t *testing.T) {
	s, _ := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodGet, "/admin/extension", nil, false)
	if rec.Code != 200 {
		t.Fatalf("extension zip: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("unexpected content type: %s", rec.Header().Get("Content-Type"))
	}
	zipBytes := rec.Body.Bytes()
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range reader.File {
		names[f.Name] = true
		if strings.Contains(f.Name, "\\") {
			t.Fatalf("zip entry uses backslashes: %s", f.Name)
		}
	}
	for _, required := range []string{
		"muse2api-extension/manifest.json",
		"muse2api-extension/popup.html",
		"muse2api-extension/popup.js",
		"muse2api-extension/_locales/en/messages.json",
	} {
		if !names[required] {
			t.Fatalf("zip missing %s", required)
		}
	}
	rec = doRequest(t, s, http.MethodGet, "/admin/extension/files", nil, false)
	var listing struct {
		Files []string `json:"files"`
	}
	decodeJSON(t, rec, &listing)
	if len(listing.Files) < 5 {
		t.Fatalf("expected at least five extension files, got %v", listing.Files)
	}
	for i := 1; i < len(listing.Files); i++ {
		if listing.Files[i-1] > listing.Files[i] {
			t.Fatalf("extension file list is not sorted")
		}
	}
}

func TestTaskAdminEndpoints(t *testing.T) {
	s, _ := newTestServer(t, "")
	task, _ := s.store.CreateTask("image", "one", func(t *store.Task) { t.Status = "completed" })
	rec := doRequest(t, s, http.MethodGet, "/admin/tasks?limit=1", nil, true)
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	decodeJSON(t, rec, &list)
	if len(list.Tasks) != 1 {
		t.Fatalf("expected one task")
	}
	rec = doRequest(t, s, http.MethodDelete, "/admin/tasks/"+task.ID, nil, true)
	if rec.Code != 200 {
		t.Fatalf("delete task: %d", rec.Code)
	}
	s.store.CreateTask("image", "two", func(t *store.Task) { t.Status = "completed" })
	rec = doRequest(t, s, http.MethodPost, "/admin/tasks/clear", map[string]any{"keep": 0}, true)
	if rec.Code != 200 {
		t.Fatalf("clear tasks: %d", rec.Code)
	}
	var cleared struct {
		Removed int `json:"removed"`
	}
	decodeJSON(t, rec, &cleared)
	if cleared.Removed != 1 {
		t.Fatalf("expected one removed task, got %d", cleared.Removed)
	}
}

func TestUpdateCheckWithoutRepo(t *testing.T) {
	s, _ := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodGet, "/admin/update/check", nil, false)
	if rec.Code != 200 {
		t.Fatalf("update check: %d %s", rec.Code, rec.Body.String())
	}
	var status struct {
		RepoURL      string `json:"repo_url"`
		LocalVersion string `json:"local_version"`
		HasUpdate    bool   `json:"has_update"`
		UpToDate     bool   `json:"up_to_date"`
	}
	decodeJSON(t, rec, &status)
	if status.RepoURL != "" || status.LocalVersion == "" || status.HasUpdate || !status.UpToDate {
		t.Fatalf("unexpected update status: %+v", status)
	}
}
