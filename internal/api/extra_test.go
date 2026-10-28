package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/muse2api/muse2api/internal/store"
	"github.com/muse2api/muse2api/internal/web"
)

func TestRotateAPIKeyTakesEffectImmediately(t *testing.T) {
	s, cfg := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodPost, "/admin/apikey/rotate", nil, true)
	if rec.Code != 200 {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		APIKey string `json:"api_key"`
	}
	decodeJSON(t, rec, &rotated)
	if !strings.HasPrefix(rotated.APIKey, "m2a_") || len(rotated.APIKey) < 20 {
		t.Fatalf("invalid rotated key: %q", rotated.APIKey)
	}
	if rec := doRequest(t, s, http.MethodGet, "/admin/status", nil, true); rec.Code != 401 {
		t.Fatalf("old key should be rejected, got %d", rec.Code)
	}
	req, _ := http.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+rotated.APIKey)
	rec = newRecorderFor(req, s)
	if rec.Code != 200 {
		t.Fatalf("new key should be accepted, got %d", rec.Code)
	}
	data, err := os.ReadFile(filepath.Join(cfg.BaseDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "MUSE2API_KEY="+rotated.APIKey) {
		t.Fatalf(".env was not updated:\n%s", data)
	}
}

func newRecorderFor(req *http.Request, s *Server) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestImageTaskB64Response(t *testing.T) {
	s, cfg := newTestServer(t, "")
	filename := "stored.webp"
	content := []byte("stored-image-bytes")
	if err := os.WriteFile(filepath.Join(cfg.MediaDir(), filename), content, 0o644); err != nil {
		t.Fatal(err)
	}
	task, _ := s.store.CreateTask("image", "stored prompt")
	s.store.UpdateTask(task.ID, func(t *store.Task) {
		t.Status = "completed"
		t.Progress = 100
		t.ResponseFormat = "b64_json"
		t.Size = "1:1"
		t.URL = s.cfg.MediaURL(filename)
		t.Result = &store.GenerationResult{Filename: filename, Size: int64(len(content)), Kind: "image", URL: s.cfg.MediaURL(filename)}
	})
	rec := doRequest(t, s, http.MethodGet, "/v1/images/tasks/"+task.ID, nil, true)
	if rec.Code != 200 {
		t.Fatalf("get task: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Data   []struct {
			B64  string `json:"b64_json"`
			URL  string `json:"url"`
			Size string `json:"size"`
		} `json:"data"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Status != "completed" || len(resp.Data) != 1 {
		t.Fatalf("unexpected task response: %s", rec.Body.String())
	}
	if resp.Data[0].URL != "" || resp.Data[0].B64 == "" {
		t.Fatalf("b64 response must not include a URL: %s", rec.Body.String())
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Data[0].B64)
	if err != nil || !bytes.Equal(decoded, content) {
		t.Fatalf("b64 payload mismatch: %v", err)
	}
	if resp.Data[0].Size != "1:1" {
		t.Fatalf("size field not preserved")
	}
}

func TestExtensionZipIsCompleteMirror(t *testing.T) {
	s, _ := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodGet, "/admin/extension", nil, false)
	zipBytes := rec.Body.Bytes()
	reader, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	archived := map[string]bool{}
	for _, f := range reader.File {
		archived[f.Name] = true
	}
	fsys := web.ExtensionFS()
	var source []string
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		source = append(source, "muse2api-extension/"+filepath.ToSlash(path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(source)
	if len(source) != len(archived) {
		t.Fatalf("archive has %d entries, source has %d", len(archived), len(source))
	}
	for _, name := range source {
		if !archived[name] {
			t.Fatalf("archive missing %s", name)
		}
	}
}

func TestKeepaliveBusyAndForceRun(t *testing.T) {
	s, _ := newTestServer(t, "")
	s.store.AddAccount(map[string]string{"hatch_sess": "s", "hatch_vml": "v"}, "a", nil)
	started := make(chan struct{})
	release := make(chan struct{})
	s.keepalive.probeFn = func(ctx context.Context, accountID string, checkQuota bool) map[string]any {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return map[string]any{"ok": true, "id": accountID}
	}
	resultCh := make(chan map[string]any, 1)
	go func() {
		res, _ := s.keepalive.RunAll(context.Background(), true)
		resultCh <- res
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not start")
	}
	busy, err := s.keepalive.RunAll(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if busy["status"] != "busy" {
		t.Fatalf("expected busy marker, got %+v", busy)
	}
	close(release)
	select {
	case res := <-resultCh:
		if res["refreshed_count"] != 1 {
			t.Fatalf("expected one refreshed account, got %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("keepalive did not finish")
	}
}

func TestCookieHelperAndMissingResources(t *testing.T) {
	s, _ := newTestServer(t, "")
	rec := doRequest(t, s, http.MethodGet, "/admin/cookie-helper", nil, false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "muse2api cookie-helper") {
		t.Fatalf("cookie helper document is missing: %d %s", rec.Code, rec.Body.String())
	}
	rec = doRequest(t, s, http.MethodPost, "/admin/accounts/missing/test", nil, true)
	if rec.Code != 404 {
		t.Fatalf("missing account test: %d", rec.Code)
	}
	rec = doRequest(t, s, http.MethodPost, "/admin/accounts/missing/quota", nil, true)
	if rec.Code != 404 {
		t.Fatalf("missing account quota: %d", rec.Code)
	}
	rec = doRequest(t, s, http.MethodGet, "/v1/videos/missing", nil, true)
	if rec.Code != 404 {
		t.Fatalf("missing video task: %d", rec.Code)
	}
	rec = doRequest(t, s, http.MethodGet, "/v1/images/tasks/missing", nil, true)
	if rec.Code != 404 {
		t.Fatalf("missing image task: %d", rec.Code)
	}
}

func TestAccountPatchAndDelete(t *testing.T) {
	s, _ := newTestServer(t, "")
	acc, _ := s.store.AddAccount(map[string]string{"hatch_sess": "s", "hatch_vml": "v"}, "old", nil)
	rec := doRequest(t, s, http.MethodPatch, "/admin/accounts/"+acc.ID, map[string]any{"label": "new", "enabled": false}, true)
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body.String())
	}
	var patched map[string]any
	decodeJSON(t, rec, &patched)
	if patched["label"] != "new" || patched["enabled"] != false || patched["cookie_count"] != float64(2) {
		t.Fatalf("unexpected patch response: %s", rec.Body.String())
	}
	if _, leaked := patched["cookies"]; leaked {
		t.Fatalf("cookies must not be returned")
	}
	rec = doRequest(t, s, http.MethodDelete, "/admin/accounts/"+acc.ID, nil, true)
	if rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	rec = doRequest(t, s, http.MethodDelete, "/admin/accounts/"+acc.ID, nil, true)
	if rec.Code != 404 {
		t.Fatalf("second delete should be 404, got %d", rec.Code)
	}
}

func TestImageAndVideoReferencesJSONRoundTrip(t *testing.T) {
	// JSON decoding of both the image and video request shapes should preserve
	// ordered references without transforming them.
	var img ImageRequest
	if err := json.Unmarshal([]byte(`{"prompt":"x","images":[{"url":"a"},{"image_url":{"url":"b"}}]}`), &img); err != nil {
		t.Fatal(err)
	}
	refs, err := img.references()
	if err != nil || len(refs) != 2 || refs[0] != "a" || refs[1] != "b" {
		t.Fatalf("image refs: %#v %v", refs, err)
	}
	var video VideoRequest
	if err := json.Unmarshal([]byte(`{"prompt":"x","images":["a","b"]}`), &video); err != nil {
		t.Fatal(err)
	}
	refs, err = video.references()
	if err != nil || len(refs) != 2 {
		t.Fatalf("video refs: %#v %v", refs, err)
	}
}
