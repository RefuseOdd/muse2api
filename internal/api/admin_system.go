package api

import (
	"archive/zip"
	"bytes"
	"errors"
	"github.com/muse2api/muse2api/internal/store"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/web"
)

func (s *Server) handleAPIKey(w http.ResponseWriter, r *http.Request) {
	base := s.cfg.BaseURL()
	modelsURL := ""
	mediaURL := "/v1/media/{name}"
	if base != "" {
		modelsURL = base + "/models"
		mediaURL = base + "/media/{name}"
	}
	writeJSON(w, 200, map[string]any{
		"api_key": s.APIKey(), "base_url": base, "models_url": modelsURL, "media_url": mediaURL,
	})
}

func (s *Server) handleRotateAPIKey(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ExternalKey {
		writeError(w, r, httpError(409, "API key is supplied by the environment; change MUSE2API_KEY in deployment configuration"))
		return
	}
	s.apiKeyMu.Lock()
	old := s.apiKey
	newKey := "m2a_" + randomHex(48)
	if err := s.cfg.PersistEnv("MUSE2API_KEY", newKey); err != nil {
		s.apiKeyMu.Unlock()
		writeError(w, r, err)
		return
	}
	s.apiKey = newKey
	s.apiKeyMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"ok": true, "api_key": newKey, "previous": old,
		"message": "A new key was generated and is active immediately; the old key no longer works.",
	})
}

const cookieHelperDoc = `muse2api cookie helper
======================

The easiest option is the bundled Chrome extension:
  1. Open the admin console, Get Cookies tab, and download the extension ZIP.
  2. Load it unpacked at chrome://extensions (Developer mode).
  3. Sign in to https://muse.ai/ in that browser.
  4. Paste your service URL and API key, then click "Read and import".

Command-line option (uses this same binary)
-------------------------------------------
  muse2api cookie-helper --base http://your-server:18610 --key m2a_xxx --label muse-01

The helper launches a separate Chrome window with a temporary profile, waits for you
to sign in to muse.ai, reads the HttpOnly session cookies through the Chrome DevTools
Protocol, and uploads them to the account pool. It never modifies your daily browser
profile. Run "muse2api cookie-helper --help" for all options.

Only hatch_sess and hatch_vml are required. hatch_gw is an optional routing cache and
is not needed for an import.
`

func (s *Server) handleCookieHelper(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("download") != "" && r.URL.Query().Get("download") != "0" {
		w.Header().Set("Content-Disposition", `attachment; filename="muse2api-cookie-helper.txt"`)
	}
	_, _ = w.Write([]byte(cookieHelperDoc))
}

func (s *Server) handleExtensionZip(w http.ResponseWriter, r *http.Request) {
	fsys := web.ExtensionFS()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		name := "muse2api-extension/" + filepath.ToSlash(path)
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o644)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		_, err = writer.Write(data)
		return err
	})
	if err != nil {
		writeError(w, r, httpError(500, "failed to build extension archive: %v", err))
		return
	}
	if err := zw.Close(); err != nil {
		writeError(w, r, httpError(500, "failed to build extension archive: %v", err))
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="muse2api-extension.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleExtensionFiles(w http.ResponseWriter, r *http.Request) {
	fsys := web.ExtensionFS()
	files := []string{}
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		writeError(w, r, httpError(500, "failed to list extension files: %v", err))
		return
	}
	sort.Strings(files)
	writeJSON(w, 200, map[string]any{"files": files})
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	force := false
	if v := strings.ToLower(r.URL.Query().Get("force")); v == "true" || v == "1" {
		force = true
	}
	writeJSON(w, 200, s.update.Check(force))
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.ContentLength != 0 {
		decoded, err := readJSONMap(w, r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		body = decoded
	}
	res, err := s.update.Upgrade(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	restart := true
	if v, ok := body["restart"].(bool); ok {
		restart = v
	}
	if restart {
		go func() {
			time.Sleep(500 * time.Millisecond)
			s.eng.Stop()
			os.Exit(0)
		}()
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleRepoPush(w http.ResponseWriter, r *http.Request) {
	body, err := readJSONMap(w, r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	message := stringValue(body["message"])
	token := stringValue(body["github_token"])
	res, err := s.update.Push(message, token)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, 200, map[string]any{"tasks": s.store.ListTasks(limit)})
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	ok, err := s.store.DeleteTask(splitPathValue(r, "id"))
	if errors.Is(err, store.ErrActiveTask) {
		err = httpError(409, "task is still active")
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, httpError(404, "task does not exist"))
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true})
}
func (s *Server) handleClearTasks(w http.ResponseWriter, r *http.Request) {
	keep := 0
	if r.ContentLength != 0 {
		body, err := readJSONMap(w, r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if value, exists := body["keep"]; exists {
			f, ok := value.(float64)
			if !ok || f < 0 || f >= float64(int(^uint(0)>>1)) || math.Trunc(f) != f {
				writeError(w, r, httpError(422, "keep must be a non-negative integer"))
				return
			}
			keep = int(f)
		}
	}
	removed, err := s.store.ClearTasks(keep)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"removed": removed})
}

func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	items := []map[string]any{}
	entries, err := os.ReadDir(s.cfg.MediaDir())
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			kind := "image"
			if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
				kind = "video"
			}
			items = append(items, map[string]any{
				"name": entry.Name(), "url": s.cfg.MediaURL(entry.Name()),
				"bytes": info.Size(), "mtime": info.ModTime().Unix(), "kind": kind,
			})
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i]["mtime"].(int64) > items[j]["mtime"].(int64)
	})
	writeJSON(w, 200, map[string]any{"media": items, "count": len(items)})
}

func badMediaName(name string) bool {
	return strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..")
}

func (s *Server) handleMediaDeleteBulk(w http.ResponseWriter, r *http.Request) {
	body, err := readJSONMap(w, r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	raw := body["names"]
	var names []string
	switch v := raw.(type) {
	case string:
		names = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				names = append(names, s)
			}
		}
	default:
		writeError(w, r, httpError(400, "names must be an array of filenames"))
		return
	}
	removed := 0
	errs := []string{}
	for _, name := range names {
		if badMediaName(name) {
			errs = append(errs, "illegal filename: "+name)
			continue
		}
		p := filepath.Join(s.cfg.MediaDir(), name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if err := os.Remove(p); err != nil {
				errs = append(errs, name+": "+err.Error())
			} else {
				removed++
			}
		}
	}
	writeJSON(w, 200, map[string]any{"removed": removed, "errors": errs})
}

func (s *Server) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
	name := splitPathValue(r, "name")
	if badMediaName(name) {
		writeError(w, r, httpError(400, "illegal filename"))
		return
	}
	p := filepath.Join(s.cfg.MediaDir(), name)
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		writeError(w, r, httpError(404, "file does not exist"))
		return
	}
	if err := os.Remove(p); err != nil {
		writeError(w, r, httpError(500, "delete failed: %v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "deleted": name})
}

func (s *Server) handleMediaFile(w http.ResponseWriter, r *http.Request) {
	name := splitPathValue(r, "name")
	if name == "" || badMediaName(name) {
		writeError(w, r, httpError(400, "illegal filename"))
		return
	}
	p := filepath.Join(s.cfg.MediaDir(), name)
	if st, err := os.Stat(p); err != nil || st.IsDir() {
		writeError(w, r, httpError(404, "file does not exist"))
		return
	}
	http.ServeFile(w, r, p)
}

func readDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }
