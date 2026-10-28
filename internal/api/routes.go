package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/web"
)

func corsMiddleware(next http.Handler, origins string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")
		for _, allowed := range strings.Split(origins, ",") {
			allowed = strings.TrimSpace(allowed)
			if allowed == "*" {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				break
			}
			if origin != "" && origin == allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, Idempotency-Key, *")
		w.Header().Set("Access-Control-Expose-Headers", "*")
		w.Header().Set("Access-Control-Max-Age", "600")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, r, httpError(500, "internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	// Public service and media endpoints.
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /readyz", s.handleReady)
	s.mux.HandleFunc("GET /v1/media/{name}", s.handleMediaFile)
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /admin", s.handleIndex)

	// OpenAI-compatible API.
	s.mux.HandleFunc("GET /v1/models", s.requireAuth(s.handleModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.requireAuth(s.handleChatCompletions))
	s.mux.HandleFunc("POST /v1/responses", s.requireAuth(s.handleResponses))
	s.mux.HandleFunc("POST /v1/images/generations", s.requireAuth(s.handleImagesGenerations))
	s.mux.HandleFunc("POST /v1/images/edits", s.requireAuth(s.handleImagesEdits))
	s.mux.HandleFunc("POST /v1/images/tasks", s.requireAuth(s.handleCreateImageTask))
	s.mux.HandleFunc("GET /v1/images/tasks/{id}", s.requireAuth(s.handleGetImageTask))
	s.mux.HandleFunc("POST /v1/videos", s.requireAuth(s.handleCreateVideo))
	s.mux.HandleFunc("POST /v1/videos/generations", s.requireAuth(s.handleCreateVideo))
	s.mux.HandleFunc("GET /v1/videos/{id}", s.requireAuth(s.handleGetVideo))
	s.mux.HandleFunc("GET /v1/videos/generations/{id}", s.requireAuth(s.handleGetVideo))

	// Admin: overview and account pool.
	s.mux.HandleFunc("GET /admin/status", s.requireAuth(s.handleAdminStatus))
	s.mux.HandleFunc("GET /admin/accounts", s.requireAuth(s.handleListAccounts))
	s.mux.HandleFunc("POST /admin/accounts", s.requireAuth(s.handleAddAccount))
	s.mux.HandleFunc("PATCH /admin/accounts/{id}", s.requireAuth(s.handlePatchAccount))
	s.mux.HandleFunc("DELETE /admin/accounts/{id}", s.requireAuth(s.handleDeleteAccount))
	s.mux.HandleFunc("POST /admin/accounts/{id}/test", s.requireAuth(s.handleTestAccount))
	s.mux.HandleFunc("POST /admin/accounts/{id}/relogin", s.requireAuth(s.handleTestAccount))
	s.mux.HandleFunc("POST /admin/accounts/{id}/cookies", s.requireAuth(s.handleUpdateCookies))
	s.mux.HandleFunc("POST /admin/accounts/{id}/quota", s.requireAuth(s.handleQuotaAccount))
	s.mux.HandleFunc("POST /admin/accounts/{id}/keepalive", s.requireAuth(s.handleKeepaliveOne))
	s.mux.HandleFunc("POST /admin/accounts/keepalive", s.requireAuth(s.handleKeepaliveAll))
	s.mux.HandleFunc("GET /admin/keepalive/status", s.requireAuth(s.handleKeepaliveStatus))
	s.mux.HandleFunc("POST /admin/relogin", s.requireAuth(s.handleRelogin))
	s.mux.HandleFunc("POST /admin/quota", s.requireAuth(s.handleQuotaAny))

	// Admin: key, cookie onboarding, tasks, media.
	s.mux.HandleFunc("GET /admin/apikey", s.requireAuth(s.handleAPIKey))
	s.mux.HandleFunc("POST /admin/apikey/rotate", s.requireAuth(s.handleRotateAPIKey))
	s.mux.HandleFunc("GET /admin/cookie-helper", s.handleCookieHelper)
	s.mux.HandleFunc("GET /admin/extension", s.handleExtensionZip)
	s.mux.HandleFunc("GET /admin/extension/files", s.handleExtensionFiles)
	s.mux.HandleFunc("GET /admin/tasks", s.requireAuth(s.handleTasks))
	s.mux.HandleFunc("DELETE /admin/tasks/{id}", s.requireAuth(s.handleDeleteTask))
	s.mux.HandleFunc("POST /admin/tasks/clear", s.requireAuth(s.handleClearTasks))
	s.mux.HandleFunc("GET /admin/media", s.requireAuth(s.handleMediaList))
	s.mux.HandleFunc("POST /admin/media/delete", s.requireAuth(s.handleMediaDeleteBulk))
	s.mux.HandleFunc("DELETE /admin/media/{name}", s.requireAuth(s.handleMediaDelete))

	// Admin: update and repository.
	s.mux.HandleFunc("GET /admin/update/check", s.handleUpdateCheck)
	s.mux.HandleFunc("GET /admin/repo/status", s.handleUpdateCheck)
	s.mux.HandleFunc("POST /admin/update/upgrade", s.requireAuth(s.handleUpgrade))
	s.mux.HandleFunc("POST /admin/repo/pull", s.requireAuth(s.handleUpgrade))
	s.mux.HandleFunc("POST /admin/repo/push", s.requireAuth(s.handleRepoPush))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(web.AdminHTML)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "time": time.Now().Unix()})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	enabled := 0
	for _, a := range s.store.ListAccounts() {
		if a.Enabled {
			enabled++
		}
	}
	status := "no_account"
	if enabled > 0 {
		status = "ready"
	}
	writeJSON(w, 200, map[string]any{
		"status":          status,
		"accounts":        enabled,
		"browser_running": s.eng.BrowserRunning(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"object": "list", "data": models})
}

// SplitPathValue mirrors PathValue for small helpers.
func splitPathValue(r *http.Request, name string) string {
	return strings.TrimSpace(r.PathValue(name))
}
