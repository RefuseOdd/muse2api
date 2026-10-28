package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/store"
)

func (s *Server) handleCreateVideo(w http.ResponseWriter, r *http.Request) {
	var req VideoRequest
	if err := decodeJSONBody(w, r, &req, 64<<20); err != nil {
		writeError(w, r, err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeError(w, r, httpError(422, "request validation failed: prompt is required"))
		return
	}
	if req.Timeout != nil && (*req.Timeout < 1 || *req.Timeout > 1800) {
		writeError(w, r, httpError(422, "timeout must be between 1 and 1800 seconds"))
		return
	}
	if req.Model == "" {
		req.Model = "muse-video"
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
	prompt := buildVideoPrompt(&req)
	timeout := s.cfg.VideoTimeout
	if req.Timeout != nil && *req.Timeout > 0 {
		timeout = *req.Timeout
	}
	s.imageMu.Lock()
	if s.store.CountActiveTasks() >= 8 {
		s.imageMu.Unlock()
		writeError(w, r, httpError(429, "the generation queue is full; retry later"))
		return
	}
	task, err := s.store.CreateTask("video", req.Prompt, func(t *store.Task) { t.APIPrompt = prompt; t.Progress = 10 })
	s.imageMu.Unlock()
	if err != nil {
		writeError(w, r, err)
		return
	}
	s.startTask(task, prompt, prepared, time.Duration(timeout)*time.Second, 15)

	writeJSON(w, 200, map[string]any{
		"id": task.ID, "task_id": task.ID, "object": "video.task",
		"status": "queued", "progress": 10, "created_at": task.CreatedAt,
	})
}

func (s *Server) handleGetVideo(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	task := s.store.GetTask(id)
	if task == nil || task.Kind != "video" {
		writeError(w, r, httpError(404, "task does not exist"))
		return
	}
	out := taskToMap(task)
	status, _ := out["status"].(string)
	switch status {
	case "succeeded", "success", "done":
		status = "completed"
		out["status"] = status
	}
	if status == "completed" {
		out["progress"] = 100
	} else if status == "processing" {
		progress := 0
		if p, ok := out["progress"].(float64); ok {
			progress = int(p)
		}
		if progress < 15 {
			out["progress"] = 15
		}
	}
	url, _ := out["url"].(string)
	if url == "" {
		if result, ok := out["result"].(map[string]any); ok {
			url, _ = result["url"].(string)
		}
	}
	if url != "" {
		out["url"] = url
		if _, ok := out["video"]; !ok {
			out["video"] = map[string]any{"url": url}
		}
	}
	writeJSON(w, 200, out)
}
