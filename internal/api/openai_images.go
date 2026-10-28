package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

func validateImageRequest(req *ImageRequest) error {
	if strings.TrimSpace(req.Prompt) == "" {
		return httpError(422, "request validation failed: prompt is required")
	}
	if req.Timeout != nil && (*req.Timeout < 1 || *req.Timeout > 600) {
		return httpError(422, "request validation failed: timeout must be between 1 and 600 seconds")
	}
	if req.ResponseFormat == "" {
		req.ResponseFormat = "url"
	}
	if req.Model == "" {
		req.Model = "muse-image"
	}
	return nil
}

func (s *Server) imageTimeout(req *ImageRequest) time.Duration {
	if req.Timeout != nil && *req.Timeout > 0 {
		return time.Duration(*req.Timeout) * time.Second
	}
	return time.Duration(s.cfg.ImageTimeout) * time.Second
}

func (s *Server) imageResponse(req *ImageRequest, res *engine.GenResult) (map[string]any, error) {
	item := map[string]any{
		"revised_prompt": req.Prompt,
		"url":            s.cfg.MediaURL(res.Filename),
		"size":           defaultString(req.Size, "auto"),
		"kind":           res.Kind,
		"bytes":          res.Size,
	}
	if req.ResponseFormat == "b64_json" {
		path := res.Path
		if path == "" {
			path = filepath.Join(s.cfg.MediaDir(), res.Filename)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		item["b64_json"] = base64.StdEncoding.EncodeToString(data)
		delete(item, "url")
	}
	return map[string]any{"created": time.Now().Unix(), "data": []any{item}}, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (s *Server) handleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	var req ImageRequest
	if err := decodeJSONBody(w, r, &req, 64<<20); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateImageRequest(&req); err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := req.references()
	if err != nil {
		writeError(w, r, err)
		return
	}
	prepared, err := prepareReferences(r.Context(), s.eng, refs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	prompt := buildImagePrompt(&req)
	timeout := s.imageTimeout(&req)
	if req.Async {
		status, body, qerr := s.queueImage(&req, prompt, prepared, "")
		if qerr != nil {
			writeError(w, r, qerr)
			return
		}
		writeJSON(w, status, body)
		return
	}
	res, _, gerr := s.RunGeneration(r.Context(), prompt, "image", timeout, "", nil, prepared, nil)
	if gerr != nil {
		writeError(w, r, mapGenerationError(gerr))
		return
	}
	body, berr := s.imageResponse(&req, res)
	if berr != nil {
		writeError(w, r, httpError(500, "failed to read generated image: %v", berr))
		return
	}
	writeJSON(w, 200, body)
}

func (s *Server) handleCreateImageTask(w http.ResponseWriter, r *http.Request) {
	var req ImageRequest
	if err := decodeJSONBody(w, r, &req, 64<<20); err != nil {
		writeError(w, r, err)
		return
	}
	if err := validateImageRequest(&req); err != nil {
		writeError(w, r, err)
		return
	}
	refs, err := req.references()
	if err != nil {
		writeError(w, r, err)
		return
	}
	prepared, err := prepareReferences(r.Context(), s.eng, refs)
	if err != nil {
		writeError(w, r, err)
		return
	}
	status, body, qerr := s.queueImage(&req, buildImagePrompt(&req), prepared, r.Header.Get("Idempotency-Key"))
	if qerr != nil {
		writeError(w, r, qerr)
		return
	}
	writeJSON(w, status, body)
}

func (s *Server) queueImage(req *ImageRequest, prompt string, references []string, idempotencyKey string) (int, map[string]any, error) {
	if len(idempotencyKey) > 256 {
		return 0, nil, httpError(400, "Idempotency-Key is too long")
	}
	keyHash := ""
	if idempotencyKey != "" {
		sum := sha256.Sum256([]byte(idempotencyKey))
		keyHash = hex.EncodeToString(sum[:])
	}
	canonical, _ := jsonMarshalNoEscape(map[string]any{"request": req, "references": references})
	sum := sha256.Sum256([]byte(canonical))
	requestHash := hex.EncodeToString(sum[:])

	s.imageMu.Lock()
	if keyHash != "" {
		if existing := s.store.FindByImageRequestKey(keyHash); existing != nil {
			if existing.ImageRequestHash != requestHash {
				s.imageMu.Unlock()
				return 0, nil, httpError(409, "Idempotency-Key was reused with a different request")
			}
			s.imageMu.Unlock()
			return 202, map[string]any{
				"id": existing.ID, "task_id": existing.ID, "object": "image.task",
				"status": existing.Status, "progress": existing.Progress, "created_at": existing.CreatedAt,
			}, nil
		}
	}
	if s.store.CountActiveTasks() >= 8 {
		s.imageMu.Unlock()
		return 0, nil, httpError(429, "the generation queue is full; retry later")
	}
	task, err := s.store.CreateTask("image", req.Prompt, func(t *store.Task) {
		t.APIPrompt = prompt
		t.Size = req.Size
		t.ResponseFormat = req.ResponseFormat
		t.Progress = 0
		t.ImageRequestKey = keyHash
		t.ImageRequestHash = requestHash
	})
	s.imageMu.Unlock()
	if err != nil {
		return 0, nil, err
	}
	s.startTask(task, prompt, references, s.imageTimeout(req), 5)

	return 202, map[string]any{
		"id": task.ID, "task_id": task.ID, "object": "image.task",
		"status": "queued", "progress": 0, "created_at": task.CreatedAt,
	}, nil
}

func roundOne(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

func (s *Server) handleGetImageTask(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	task := s.store.GetTask(id)
	if task == nil || task.Kind != "image" {
		writeError(w, r, httpError(404, "image task does not exist"))
		return
	}
	out := taskToMap(task)
	if task.Status == "completed" && task.Result != nil {
		req := &ImageRequest{
			Prompt:         task.Prompt,
			Size:           task.Size,
			ResponseFormat: defaultString(task.ResponseFormat, "url"),
		}
		res := &engine.GenResult{
			Filename: task.Result.Filename,
			Size:     task.Result.Size,
			Kind:     task.Result.Kind,
			Path:     filepath.Join(s.cfg.MediaDir(), task.Result.Filename),
		}
		body, err := s.imageResponse(req, res)
		if err != nil {
			writeError(w, r, httpError(500, "failed to read generated image: %v", err))
			return
		}
		for k, v := range body {
			out[k] = v
		}
	}
	writeJSON(w, 200, out)
}

func taskToMap(task *store.Task) map[string]any {
	raw, _ := json.Marshal(task)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if _, ok := out["result"]; !ok {
		out["result"] = nil
	}
	return out
}

// handleImagesEdits accepts both multipart/form-data and application/json.
func (s *Server) handleImagesEdits(w http.ResponseWriter, r *http.Request) {
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	var req ImageRequest
	var prepared []string
	prompt := ""
	if strings.Contains(contentType, "multipart/form-data") {
		parsed, refs, err := parseMultipartEditsReader(w, r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		req = *parsed
		if strings.TrimSpace(req.Prompt) == "" {
			req.Prompt = "Create an image based on this reference image"
		}
		req.Images = stringsToAny(refs)
		prepared, err = prepareReferences(r.Context(), s.eng, refs)
		if err != nil {
			writeError(w, r, err)
			return
		}
		prompt = buildImagePrompt(&req)
	} else {
		body, err := readJSONMap(w, r)
		if err != nil {
			writeError(w, r, err)
			return
		}
		req = ImageRequest{
			Prompt:         stringValue(body["prompt"]),
			Model:          defaultString(stringValue(body["model"]), "muse-image"),
			Size:           stringValue(body["size"]),
			AspectRatio:    stringValue(body["aspect_ratio"]),
			ResponseFormat: defaultString(stringValue(body["response_format"]), "url"),
			Extra:          stringValue(body["extra"]),
			Image:          body["image"],
			ImageURL:       body["image_url"],
			ReferenceImage: stringValue(body["reference_image"]),
			Async:          boolValue(body["async"]),
		}
		if raw, ok := body["timeout"]; ok && raw != nil {
			if f, ok := raw.(float64); ok {
				v := int(f)
				req.Timeout = &v
			}
		}
		images := body["images"]
		if images != nil {
			list, ok := images.([]any)
			if !ok {
				writeError(w, r, httpError(400, "images must be an array"))
				return
			}
			req.Images = list
		}
		legacy := []any{req.ReferenceImage, req.ImageURL, req.Image}
		refs, rerr := referenceList(req.Images, legacy, "images")
		if rerr != nil {
			writeError(w, r, rerr)
			return
		}
		req.Images = stringsToAny(refs)
		req.ReferenceImage = ""
		req.Image = nil
		req.ImageURL = nil
		if strings.TrimSpace(req.Prompt) == "" {
			req.Prompt = "Create an image based on this reference image"
		}
		prepared, err = prepareReferences(r.Context(), s.eng, refs)
		if err != nil {
			writeError(w, r, err)
			return
		}
		prompt = buildImagePrompt(&req)
	}

	// Keep the original prompt in revised_prompt but use the composed prompt
	// for generation, matching the OpenAI image endpoints.
	genReq := req
	genReq.Images = nil
	if err := validateImageRequest(&genReq); err != nil {
		writeError(w, r, err)
		return
	}
	timeout := s.imageTimeout(&genReq)
	if genReq.Async {
		status, body, qerr := s.queueImage(&genReq, prompt, prepared, "")
		if qerr != nil {
			writeError(w, r, qerr)
			return
		}
		writeJSON(w, status, body)
		return
	}
	res, _, gerr := s.RunGeneration(r.Context(), prompt, "image", timeout, "", nil, prepared, nil)
	if gerr != nil {
		writeError(w, r, mapGenerationError(gerr))
		return
	}
	body, berr := s.imageResponse(&genReq, res)
	if berr != nil {
		writeError(w, r, httpError(500, "failed to read generated image: %v", berr))
		return
	}
	writeJSON(w, 200, body)
}

func stringsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

func multipartBoundary(r *http.Request) string {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return params["boundary"]
}

func parseMultipartEditsReader(w http.ResponseWriter, r *http.Request) (*ImageRequest, []string, error) {
	body := http.MaxBytesReader(w, r.Body, 64<<20)
	boundary := multipartBoundary(r)
	if boundary == "" {
		return nil, nil, httpError(400, "multipart boundary is missing")
	}
	reader := multipart.NewReader(body, boundary)
	values := map[string]string{}
	var refs []string
	fields := 0
	files := 0
	total := 0
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, httpError(400, "invalid multipart body: %v", err)
		}
		fields++
		if fields > 20 {
			_ = part.Close()
			return nil, nil, httpError(400, "too many multipart fields (maximum 20)")
		}
		name := part.FormName()
		filename := part.FileName()
		if name == "image" {
			files++
			if files > maxReferenceImages {
				_ = part.Close()
				return nil, nil, httpError(400, "image supports at most 4 reference images")
			}
			if filename == "" {
				data, rerr := io.ReadAll(io.LimitReader(part, 1<<20))
				_ = part.Close()
				if rerr != nil {
					return nil, nil, httpError(400, "failed to read multipart field")
				}
				if s := strings.TrimSpace(string(data)); s != "" {
					refs = append(refs, s)
				}
				continue
			}
			contentType := part.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "image/png"
			}
			if !strings.HasPrefix(strings.ToLower(contentType), "image/") {
				_ = part.Close()
				return nil, nil, httpError(400, "image[%d] is not an image file", files-1)
			}
			data, rerr := io.ReadAll(io.LimitReader(part, maxReferenceImageBytes+1))
			_ = part.Close()
			if rerr != nil {
				return nil, nil, httpError(400, "failed to read uploaded reference image")
			}
			if len(data) > maxReferenceImageBytes {
				return nil, nil, httpError(400, "image[%d] exceeds the 10 MiB limit", files-1)
			}
			total += len(data)
			if total > maxReferenceTotalBytes {
				return nil, nil, httpError(400, "reference images exceed the 24 MiB total limit")
			}
			refs = append(refs, "data:"+contentType+";base64,"+base64.StdEncoding.EncodeToString(data))
			continue
		}
		data, rerr := io.ReadAll(io.LimitReader(part, 1<<20))
		_ = part.Close()
		if rerr != nil {
			return nil, nil, httpError(400, "failed to read multipart field")
		}
		values[name] = string(data)
	}
	req := &ImageRequest{
		Prompt:         values["prompt"],
		Model:          defaultString(values["model"], "muse-image"),
		Size:           values["size"],
		AspectRatio:    values["aspect_ratio"],
		ResponseFormat: defaultString(values["response_format"], "url"),
		Extra:          values["extra"],
		Async:          truthy(values["async"]),
	}
	if t := strings.TrimSpace(values["timeout"]); t != "" {
		if v, err := strconv.Atoi(t); err == nil {
			req.Timeout = &v
		}
	}
	return req, refs, nil
}

func readJSONMap(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	var out map[string]any
	if err := decodeJSONBody(w, r, &out, 64<<20); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, httpError(422, "request body must be an object")
	}
	return out, nil
}

func stringValue(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolValue(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	if s, ok := v.(string); ok {
		return truthy(s)
	}
	return false
}

func truthy(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	return s == "true" || s == "1" || s == "yes" || s == "on"
}
