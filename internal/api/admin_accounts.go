package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

type batchEntry struct {
	Label   string
	Cookies map[string]string
}

func parseCookieText(text string) map[string]string {
	text = strings.TrimSpace(text)
	out := map[string]string{}
	if text == "" {
		return out
	}
	if strings.HasPrefix(text, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(text), &obj) == nil {
			for k, v := range obj {
				out[k] = stringValue(v)
			}
			return out
		}
	}
	for _, part := range splitCookieParts(text) {
		part = strings.TrimSpace(part)
		if part == "" || !strings.Contains(part, "=") {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		k := strings.TrimSpace(kv[0])
		if k != "" {
			out[k] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func splitCookieParts(text string) []string {
	replacer := strings.NewReplacer("\r\n", "\n", "\r", "\n", ";", "\n")
	return strings.Split(replacer.Replace(text), "\n")
}

func parseBatch(text string) []batchEntry {
	var out []batchEntry
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		label, body := "", line
		if idx := strings.Index(line, "|"); idx >= 0 {
			head := strings.TrimSpace(line[:idx])
			if !strings.Contains(head, "=") {
				label = head
				body = strings.TrimSpace(line[idx+1:])
			}
		}
		cookies := parseCookieText(body)
		if len(cookies) > 0 {
			out = append(out, batchEntry{Label: label, Cookies: cookies})
		}
	}
	return out
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	accounts := s.store.ListAccounts()
	mediaCount := 0
	if entries, err := readDir(s.cfg.MediaDir()); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				mediaCount++
			}
		}
	}
	writeJSON(w, 200, map[string]any{
		"accounts":          accounts,
		"stats":             s.store.Stats(),
		"tasks":             s.store.ListTasks(20),
		"media_count":       mediaCount,
		"browser_running":   s.eng.BrowserRunning(),
		"essential_cookies": engine.EssentialCookies,
		"base_url":          s.cfg.BaseURL(),
		"config": map[string]any{
			"site":          s.cfg.SiteURL,
			"cdp_port":      s.cfg.CDPPort,
			"image_timeout": s.cfg.ImageTimeout,
			"video_timeout": s.cfg.VideoTimeout,
			"host":          s.cfg.Host,
			"port":          s.cfg.Port,
			"public_base":   s.cfg.PublicURL,
		},
	})
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"accounts": s.store.ListAccounts(), "stats": s.store.Stats()})
}

func (s *Server) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var req AccountRequest
	if err := decodeJSONBody(w, r, &req, 8<<20); err != nil {
		writeError(w, r, err)
		return
	}
	added := []map[string]any{}
	var seen []map[string]string
	if strings.TrimSpace(req.Batch) != "" {
		for _, entry := range parseBatch(req.Batch) {
			acc, err := s.store.AddAccount(entry.Cookies, entry.Label, nil)
			if err != nil {
				writeError(w, r, err)
				return
			}
			seen = append(seen, entry.Cookies)
			added = append(added, map[string]any{
				"id": acc.ID, "label": acc.Label, "cookie_count": len(entry.Cookies), "expires_at": acc.ExpiresAt,
			})
		}
	}
	cookies := map[string]string{}
	for k, v := range req.Cookies {
		cookies[k] = v
	}
	if req.CookieHeader != "" {
		for k, v := range parseCookieText(req.CookieHeader) {
			cookies[k] = v
		}
	}
	if len(cookies) > 0 {
		acc, err := s.store.AddAccount(cookies, req.Label, req.Expires)
		if err != nil {
			writeError(w, r, err)
			return
		}
		seen = append(seen, cookies)
		added = append(added, map[string]any{
			"id": acc.ID, "label": acc.Label, "cookie_count": len(cookies), "expires_at": acc.ExpiresAt,
		})
	}
	if len(added) == 0 {
		writeError(w, r, httpError(400, "no cookies could be parsed; check the format"))
		return
	}
	var missing []string
	for _, name := range engine.EssentialCookies {
		found := false
		for _, cookieSet := range seen {
			if cookieSet[name] != "" {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	warning := ""
	if len(missing) > 0 {
		warning = "Missing core cookies: " + strings.Join(missing, ", ") + "; this account may not be able to generate"
	}
	writeJSON(w, 200, map[string]any{
		"added": added, "count": len(added), "essential_missing": missing, "warning": warning,
	})
}

func (s *Server) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	var req AccountPatch
	if err := decodeJSONBody(w, r, &req, 1<<20); err != nil {
		writeError(w, r, err)
		return
	}
	acc, ok, err := s.store.UpdateLabelEnabled(id, req.Label, req.Enabled)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, httpError(404, "account does not exist"))
		return
	}
	view := acc.View()
	writeJSON(w, 200, view)
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	ok, err := s.store.DeleteAccount(id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, httpError(404, "account does not exist"))
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": true, "id": id})
}

func (s *Server) handleTestAccount(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	res, err := s.keepalive.TestAccount(r.Context(), id)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleUpdateCookies(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	body, err := readJSONMap(w, r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	cookies := map[string]string{}
	if raw, ok := body["cookies"].(map[string]any); ok {
		for k, v := range raw {
			cookies[k] = stringValue(v)
		}
	}
	if header, ok := body["cookie_header"].(string); ok && header != "" {
		for k, v := range parseCookieText(header) {
			cookies[k] = v
		}
	}
	if len(cookies) == 0 {
		writeError(w, r, httpError(400, "no cookies were parsed"))
		return
	}
	exp := map[string]int64{}
	if raw, ok := body["expires"].(map[string]any); ok {
		for k, v := range raw {
			if f, ok := v.(float64); ok && int64(f) > 0 {
				exp[k] = int64(f)
			}
		}
	}
	acc := s.store.GetAccount(id)
	if acc == nil {
		writeError(w, r, httpError(404, "account does not exist"))
		return
	}
	if len(exp) == 0 {
		exp = acc.CookiesExp
	}
	updated, ok, err := s.store.UpdateCookies(id, store.CookieUpdate{
		Cookies:     cookies,
		CookiesExp:  exp,
		HasExpiry:   true,
		ResetAnchor: true,
		Note:        "Cookies updated",
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	if !ok {
		writeError(w, r, httpError(404, "account does not exist"))
		return
	}
	expires := any(nil)
	if updated.ExpiresAt != nil {
		expires = *updated.ExpiresAt
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "id": id, "cookie_count": len(cookies), "expires_at": expires,
	})
}

func (s *Server) handleQuotaAccount(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	q, err := s.keepalive.ReadQuota(r.Context(), id)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			writeError(w, r, he)
			return
		}
		if engine.IsAuthError(err) {
			writeError(w, r, httpError(401, "%s", err.Error()))
			return
		}
		writeError(w, r, httpError(502, "quota query failed: %v", err))
		return
	}
	writeJSON(w, 200, q)
}

func (s *Server) handleKeepaliveOne(w http.ResponseWriter, r *http.Request) {
	id := splitPathValue(r, "id")
	if s.store.GetAccount(id) == nil {
		writeError(w, r, httpError(404, "account does not exist"))
		return
	}
	res := s.keepalive.probeAccountSync(r.Context(), id, false)
	writeJSON(w, 200, res)
}

func (s *Server) handleKeepaliveAll(w http.ResponseWriter, r *http.Request) {
	force := true
	if v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("force"))); v == "false" || v == "0" {
		force = false
	}
	res, err := s.keepalive.RunAll(r.Context(), force)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) handleKeepaliveStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.keepalive.Status())
}

func (s *Server) handleRelogin(w http.ResponseWriter, r *http.Request) {
	acc, err := s.store.PickAccount("", true, false, nil, true)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if acc == nil {
		writeError(w, r, httpError(400, "no available accounts"))
		return
	}
	wkr, err := s.eng.Acquire(r.Context())
	if err != nil {
		writeError(w, r, httpError(502, "browser is busy or unavailable: %v", err))
		return
	}
	defer s.eng.Release(wkr)
	defer wkr.ClosePage()
	lease, err := s.pool.Acquire(r.Context(), acc.ID, "", false, false, 60*time.Second, false)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer s.pool.Release(lease.ID)
	refreshed, err := s.renewAndPersist(r.Context(), lease.ID, true, true)
	if err != nil {
		writeError(w, r, mapGenerationError(err))
		return
	}
	if refreshed == nil {
		writeError(w, r, httpError(404, "account no longer exists"))
		return
	}
	lease = refreshed
	if err := s.eng.StartContext(r.Context()); err != nil {
		writeError(w, r, httpError(502, "%v", err))
		return
	}
	if _, err := wkr.Refresh(r.Context(), lease.Cookies, lease.CookiesExp); err != nil {
		writeError(w, r, httpError(502, "%v", err))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account": acc.ID})
}

func (s *Server) handleQuotaAny(w http.ResponseWriter, r *http.Request) {
	acc, err := s.store.PickAccount("", true, false, nil, true)
	if err != nil {
		writeError(w, r, err)
		return
	}
	if acc == nil {
		writeError(w, r, httpError(400, "no available accounts"))
		return
	}
	q, err := s.keepalive.ReadQuota(r.Context(), acc.ID)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			writeError(w, r, he)
			return
		}
		if engine.IsAuthError(err) {
			writeError(w, r, httpError(401, "%s", err.Error()))
			return
		}
		writeError(w, r, httpError(502, "quota query failed: %v", err))
		return
	}
	writeJSON(w, 200, q)
}
