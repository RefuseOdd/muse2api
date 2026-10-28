package api

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

type KeepaliveManager struct {
	s *Server

	runMu sync.Mutex

	stateMu    sync.Mutex
	running    bool
	lastRun    *int64
	lastResult any
	nextRun    *int64

	// Test seam. Production leaves this nil.
	probeFn func(ctx context.Context, accountID string, checkQuota bool) map[string]any
}

func NewKeepaliveManager(s *Server) *KeepaliveManager {
	return &KeepaliveManager{s: s}
}

// Start runs browser warmup once and then the 15-minute maintenance loop.
func (m *KeepaliveManager) Start(ctx context.Context) {
	go func() {
		log.Printf("auto keepalive daemon started; interval: 15 minutes")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		m.warmup(ctx)
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := m.RunAll(ctx, false); err != nil {
					log.Printf("auto keepalive cycle failed: %v", err)
				}
			}
		}
	}()
}

func (m *KeepaliveManager) Status() map[string]any {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return map[string]any{
		"running":     m.running,
		"last_run":    m.lastRun,
		"last_result": m.lastResult,
		"next_run":    m.nextRun,
	}
}

// RunAll refreshes due accounts sequentially. It returns a busy marker when a
// cycle is already running.
func (m *KeepaliveManager) RunAll(ctx context.Context, force bool) (map[string]any, error) {
	if !m.runMu.TryLock() {
		return map[string]any{"status": "busy", "message": "keepalive is already running; please wait"}, nil
	}
	defer m.runMu.Unlock()

	now := time.Now().Unix()
	m.stateMu.Lock()
	m.running = true
	m.lastRun = &now
	m.stateMu.Unlock()
	defer func() {
		m.stateMu.Lock()
		m.running = false
		m.stateMu.Unlock()
	}()

	results := []any{}
	skipped := []any{}
	accounts := m.s.store.AllEnabledForKeepalive(m.s.cfg.KeepaliveDisabledAccounts)
	for _, a := range accounts {
		lastKeepalive := int64(0)
		if a.LastKeepalive != nil {
			lastKeepalive = *a.LastKeepalive
		}
		needsRun := force || lastKeepalive == 0
		if !needsRun {
			needsRun = (now-lastKeepalive) > 6*3600 &&
				(a.ExpiresAt == nil || *a.ExpiresAt-now < 36*3600 || a.OK == nil || !*a.OK)
		}
		if !needsRun {
			skipped = append(skipped, map[string]any{"id": a.ID, "label": a.Label, "reason": "session sufficient and recently kept alive"})
			continue
		}
		log.Printf("[keepalive] silently renewing account %s (%s)...", a.Label, a.ID)
		res := m.probeAccountSync(ctx, a.ID, false)
		results = append(results, res)
		log.Printf("[keepalive] account %s result: ok=%v", a.Label, res["ok"])
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	next := time.Now().Unix() + 900
	summary := map[string]any{
		"checked_at":      now,
		"refreshed_count": len(results),
		"refreshed":       results,
		"skipped_count":   len(skipped),
		"skipped":         skipped,
	}
	m.stateMu.Lock()
	m.lastResult = summary
	m.nextRun = &next
	m.stateMu.Unlock()
	return summary, nil
}

// probeAccountSync renews one account through /api/session. Only an explicit
// 401 marks the account unhealthy.
func (m *KeepaliveManager) probeAccountSync(ctx context.Context, accountID string, checkQuota bool) map[string]any {
	if m.probeFn != nil {
		return m.probeFn(ctx, accountID, checkQuota)
	}
	acc := m.s.store.GetAccount(accountID)
	if acc == nil || len(acc.Cookies) == 0 {
		label := accountID
		if acc != nil {
			label = acc.Label
		}
		return map[string]any{"ok": false, "id": accountID, "label": label, "error": "account has no valid cookies"}
	}
	res, err := m.s.renewSession(ctx, acc.Cookies, acc.CookiesExp, true)
	if err != nil {
		ok := (*bool)(nil)
		if engine.IsAuthError(err) {
			ok = boolPtr(false)
		}
		if _, _, saveErr := m.s.store.UpdateCookies(accountID, store.CookieUpdate{ExpectedCookies: acc.Cookies, ExpectedExpiry: acc.CookiesExp, OK: ok, Note: "Keepalive unconfirmed: " + truncateNote(err.Error(), 200)}); saveErr != nil {
			err = saveErr
		}

		return map[string]any{"ok": false, "id": accountID, "label": acc.Label, "error": err.Error()}
	}
	if _, err := m.s.saveSession(acc, res, "Session valid - auto keepalive"); err != nil {
		return map[string]any{"ok": false, "id": accountID, "label": acc.Label, "error": err.Error()}
	}
	vmState := res.VMState

	quota := acc.Quota
	if checkQuota {
		if w, ok := m.s.eng.TryAcquire(); ok {
			func() {
				defer m.s.eng.Release(w)
				defer w.ClosePage()
				lease, ok := m.s.pool.TryAcquire(accountID)
				if !ok {
					return
				}
				defer m.s.pool.Release(lease.ID)
				if err := m.s.eng.StartContext(ctx); err != nil {
					log.Printf("quota read for account %s failed: %v", accountID, err)
					return
				}
				q, qerr := w.Quota(ctx, lease.Cookies, lease.CookiesExp, accountID)
				if qerr != nil {
					log.Printf("quota read for account %s failed: %v", accountID, qerr)
					return
				}
				if !q.Found {
					log.Printf("quota read for account %s returned no data; cache preserved", accountID)
					return
				}
				q.CheckedAt = time.Now().Unix()
				if _, _, err := m.s.store.SetQuota(accountID, q); err != nil {
					log.Printf("quota save: %v", err)
					return
				}
				quota = q
			}()
		}
	}
	updated := m.s.store.GetAccount(accountID)
	out := map[string]any{
		"ok":       true,
		"id":       accountID,
		"label":    acc.Label,
		"vm_id":    res.VMID,
		"vm_state": vmState,
		"wake_ok":  res.WakeOK,
		"quota":    quota,
	}
	if updated != nil && updated.ExpiresAt != nil {
		out["expires_at"] = *updated.ExpiresAt
	}
	return out
}

func (m *KeepaliveManager) warmup(ctx context.Context) {
	if m.s.eng.BrowserRunning() && m.s.eng.PreferredAccount() != "" {
		return
	}
	w, ok := m.s.eng.TryAcquire()
	if !ok {
		return
	}
	defer m.s.eng.Release(w)
	defer w.ClosePage()

	acc, err := m.s.store.PickAccount(m.s.eng.PreferredAccount(), false, false, nil, false)
	if err != nil {
		log.Printf("warmup: %v", err)
		return
	}
	if acc == nil {
		return
	}
	lease, err := m.s.pool.Acquire(ctx, acc.ID, "", false, false, 100*time.Millisecond, false)
	if err != nil || lease == nil {
		return
	}
	defer m.s.pool.Release(acc.ID)
	log.Printf("[warmup] preheating account %s (%s)...", acc.Label, acc.ID)
	refreshed, _ := m.s.renewAndPersist(ctx, acc.ID, true, false)
	if refreshed != nil {
		acc = refreshed
	}
	if err := m.s.eng.StartContext(ctx); err != nil {
		log.Printf("[warmup] failed: %v", err)
		return
	}
	if _, err := w.EnsurePage(ctx, acc.Cookies, acc.CookiesExp, acc.ID); err != nil {
		log.Printf("[warmup] page preparation failed: %v", err)
		return
	}
	log.Printf("[warmup] account %s is ready", acc.Label)
}

// TestAccount opens muse.ai to validate the session and tries to refresh quota.
func (m *KeepaliveManager) TestAccount(ctx context.Context, accountID string) (map[string]any, error) {
	acc := m.s.store.GetAccount(accountID)
	if acc == nil {
		return nil, httpError(404, "account does not exist")
	}
	if len(acc.Cookies) == 0 {
		return nil, httpError(400, "this account has no cookies")
	}
	acqCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	w, err := m.s.eng.Acquire(acqCtx)
	if err != nil {
		return nil, &engine.MuseGenerationError{Message: "timed out waiting for the browser queue"}
	}
	defer m.s.eng.Release(w)
	defer w.ClosePage()

	lease, err := m.s.pool.Acquire(ctx, accountID, "", false, false, 60*time.Second, false)
	if err != nil {
		return nil, err
	}
	defer m.s.pool.Release(lease.ID)
	acc = lease
	refreshed, rerr := m.s.renewAndPersist(ctx, acc.ID, true, false)
	if rerr != nil {
		return nil, rerr
	}
	if refreshed != nil {
		acc = refreshed
	}

	if err := m.s.eng.StartContext(ctx); err != nil {
		if saveErr := m.s.markAccount(acc, nil, "Test unconfirmed (state preserved): "+truncateNote(err.Error(), 200)); saveErr != nil {
			return nil, saveErr
		}
		return map[string]any{"ok": false, "message": truncateNote(err.Error(), 200)}, nil
	}
	if _, err := w.Refresh(ctx, acc.Cookies, acc.CookiesExp); err != nil {
		if engine.IsAuthError(err) {
			if saveErr := m.s.markAccount(acc, boolPtr(false), truncateNote(err.Error(), 200)); saveErr != nil {
				return nil, saveErr
			}
			return map[string]any{"ok": false, "message": truncateNote(err.Error(), 200)}, nil
		}
		if saveErr := m.s.markAccount(acc, nil, "Test unconfirmed (state preserved): "+truncateNote(err.Error(), 200)); saveErr != nil {
			return nil, saveErr
		}
		return map[string]any{"ok": false, "message": truncateNote(err.Error(), 200)}, nil
	}

	synced, err := m.s.syncWorkerCookies(accountID, w)
	if err != nil {
		return nil, err
	}
	acc = m.s.store.GetAccount(accountID)
	if acc == nil {
		return nil, httpError(404, "account no longer exists")
	}
	var quota *store.Quota
	quotaError := ""
	q, qerr := w.Quota(ctx, acc.Cookies, acc.CookiesExp, accountID)
	if qerr != nil {
		quotaError = fmt.Sprintf("%T; quota cache was not updated", qerr)
	} else if q.Found {
		q.CheckedAt = time.Now().Unix()
		if _, _, err := m.s.store.SetQuota(accountID, q); err != nil {
			return nil, err
		}
		quota = q
	} else {
		quotaError = "the usage panel returned no valid data; the old cache was preserved"
	}
	note := "Session valid"
	if quotaError != "" {
		note += "; quota read failed"
	}
	if saveErr := m.s.markAccount(acc, boolPtr(true), note); saveErr != nil {
		return nil, saveErr
	}
	return map[string]any{
		"ok":          true,
		"message":     "Session is valid and can generate",
		"synced":      synced,
		"quota":       quota,
		"quota_error": emptyToNil(quotaError),
	}, nil
}

// ReadQuota reads one account's usage live and caches the result.
func (m *KeepaliveManager) ReadQuota(ctx context.Context, accountID string) (*store.Quota, error) {
	acc := m.s.store.GetAccount(accountID)
	if acc == nil {
		return nil, httpError(404, "account does not exist")
	}
	if len(acc.Cookies) == 0 {
		return nil, httpError(400, "this account has no cookies")
	}
	acqCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	w, err := m.s.eng.Acquire(acqCtx)
	if err != nil {
		return nil, &engine.MuseGenerationError{Message: "timed out waiting for the browser queue"}
	}
	defer m.s.eng.Release(w)
	defer w.ClosePage()
	lease, err := m.s.pool.Acquire(ctx, accountID, "", false, false, 60*time.Second, false)
	if err != nil {
		return nil, err
	}
	defer m.s.pool.Release(lease.ID)
	acc = lease
	refreshed, rerr := m.s.renewAndPersist(ctx, acc.ID, true, false)
	if rerr != nil {
		return nil, rerr
	}
	if refreshed != nil {
		acc = refreshed
	}
	if err := m.s.eng.StartContext(ctx); err != nil {
		return nil, &engine.MuseGenerationError{Message: err.Error()}
	}
	q, err := w.Quota(ctx, acc.Cookies, acc.CookiesExp, accountID)
	if err != nil {
		if engine.IsAuthError(err) {
			if saveErr := m.s.markAccount(acc, boolPtr(false), truncateNote(err.Error(), 200)); saveErr != nil {
				return nil, saveErr
			}
		}
		return nil, err
	}
	if !q.Found {
		return nil, &engine.MuseGenerationError{Message: "the usage panel returned no valid data; the old cache was preserved"}
	}
	q.CheckedAt = time.Now().Unix()
	if _, _, err := m.s.store.SetQuota(accountID, q); err != nil {
		return nil, err
	}
	return q, nil
}

func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
