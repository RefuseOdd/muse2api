package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/muse2api/muse2api/internal/config"
	"github.com/muse2api/muse2api/internal/engine"
	"github.com/muse2api/muse2api/internal/store"
)

// Server wires configuration, persistence, the browser engine and the account
// lease pool together.
type Server struct {
	cfg   *config.Config
	store *store.Store
	eng   *engine.Engine
	pool  *AccountPool
	mux   *http.ServeMux

	apiKeyMu sync.RWMutex
	apiKey   string

	imageMu sync.Mutex

	jobCtx context.Context

	keepalive *KeepaliveManager
	update    *UpdateManager

	// Test seams. Production leaves these nil.
	runGenerationFn func(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error)
	streamChatFn    func(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) ([]engine.ChatFile, error)
	renewSessionFn  func(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error)
}

func NewServer(cfg *config.Config, st *store.Store, eng *engine.Engine) *Server {
	s := &Server{
		cfg:    cfg,
		store:  st,
		eng:    eng,
		pool:   NewAccountPool(st),
		mux:    http.NewServeMux(),
		apiKey: cfg.APIKey,
		jobCtx: context.Background(),
	}
	s.keepalive = NewKeepaliveManager(s)
	s.update = NewUpdateManager(s)
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return corsMiddleware(s.mux, s.cfg.CORSOrigins)
}

// StartBackground launches the browser warmup and keepalive daemon.
func (s *Server) StartBackground(ctx context.Context) {
	s.jobCtx = ctx
	s.keepalive.Start(ctx)
}

func (s *Server) APIKey() string {
	s.apiKeyMu.RLock()
	defer s.apiKeyMu.RUnlock()
	return s.apiKey
}

func (s *Server) SetAPIKey(key string) {
	s.apiKeyMu.Lock()
	s.apiKey = key
	s.apiKeyMu.Unlock()
}

func (s *Server) authorized(r *http.Request) bool {
	key := s.APIKey()
	if key == "" {
		return true
	}
	token := bearerToken(r.Header.Get("Authorization"))
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			token := bearerToken(r.Header.Get("Authorization"))
			msg := "missing Authorization: Bearer <key>"
			if token != "" {
				msg = "invalid API key"
			}
			writeError(w, r, httpError(401, "%s", msg))
			return
		}
		next(w, r)
	}
}

// ---------- Account lease pool ----------

// AccountPool guarantees that one account cannot be used by two concurrent
// tasks, while different accounts run in parallel.
type AccountPool struct {
	mu    sync.Mutex
	store *store.Store
	busy  map[string]bool
}

func NewAccountPool(st *store.Store) *AccountPool {
	return &AccountPool{store: st, busy: map[string]bool{}}
}

func (p *AccountPool) Acquire(ctx context.Context, accountID, preferredID string, rotate, forceRotate bool, timeout time.Duration, recordUse bool) (*store.Account, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p.mu.Lock()
		if accountID != "" {
			acc := p.store.GetAccount(accountID)
			if acc == nil || !acc.Enabled {
				p.mu.Unlock()
				return nil, &engine.MuseAuthError{Message: fmt.Sprintf("specified account %s does not exist or is disabled", accountID)}
			}
			if !p.busy[accountID] {
				p.busy[accountID] = true
				p.mu.Unlock()
				return acc, nil
			}
		} else {
			pref := preferredID
			if pref != "" && p.busy[pref] {
				pref = ""
			}
			acc, err := p.store.PickAccount(pref, rotate, forceRotate, p.busy, recordUse)
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			if acc != nil {
				p.busy[acc.ID] = true
				p.mu.Unlock()
				return acc, nil
			}
		}
		live := 0
		for _, a := range p.store.ListAccounts() {
			if a.Enabled && a.CookieCount > 0 {
				live++
			}
		}
		p.mu.Unlock()
		if live == 0 {
			return nil, &engine.MuseAuthError{Message: "no available accounts; import cookies on the admin page first"}
		}
		if time.Now().After(deadline) {
			return nil, &engine.MuseGenerationError{Message: "timed out waiting for an idle account; all accounts are busy"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *AccountPool) Release(id string) {
	if id == "" {
		return
	}
	p.mu.Lock()
	delete(p.busy, id)
	p.mu.Unlock()
}

func (p *AccountPool) Busy() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.busy)
}

// ---------- Session helpers ----------

func (s *Server) renewSession(ctx context.Context, cookies map[string]string, expires map[string]int64, wakeVM bool) (*engine.SessionRenewResult, error) {
	if s.renewSessionFn != nil {
		return s.renewSessionFn(ctx, cookies, expires, wakeVM)
	}
	return s.eng.RenewSessionHTTP(ctx, cookies, expires, wakeVM)
}

func (s *Server) renewAndPersist(ctx context.Context, accountID string, wakeVM, force bool) (*store.Account, error) {
	acc := s.store.GetAccount(accountID)
	if acc == nil || len(acc.Cookies) == 0 {
		return acc, nil
	}
	lastSync := int64(0)
	if acc.SyncedAt != nil {
		lastSync = *acc.SyncedAt
	}
	if acc.LastKeepalive != nil && *acc.LastKeepalive > lastSync {
		lastSync = *acc.LastKeepalive
	}
	now := time.Now()
	if !force && acc.OK != nil && *acc.OK && now.Unix()-lastSync < 600 {
		return acc, nil
	}
	res, err := s.renewSession(ctx, acc.Cookies, acc.CookiesExp, wakeVM)
	if err != nil {
		if engine.IsAuthError(err) {
			_, _, saveErr := s.store.UpdateCookies(accountID, store.CookieUpdate{ExpectedCookies: acc.Cookies, ExpectedExpiry: acc.CookiesExp, OK: boolPtr(false), Note: err.Error()})
			if errors.Is(saveErr, store.ErrStaleCookies) {
				return s.store.GetAccount(accountID), nil
			}
			if saveErr != nil {
				return nil, saveErr
			}
			return nil, err
		}
		log.Printf("HTTP pre-renew for account %s failed (continuing): %v", accountID, err)
		return acc, nil
	}
	updated, err := s.saveSession(acc, res, "Session healthy")
	if errors.Is(err, store.ErrStaleCookies) {
		return s.store.GetAccount(accountID), nil
	}
	return updated, err
}

func (s *Server) saveSession(acc *store.Account, res *engine.SessionRenewResult, note string) (*store.Account, error) {
	now := time.Now().Unix()
	vm := res.VMState
	if vm == "" {
		vm = "unknown"
	}
	updated, ok, err := s.store.UpdateCookies(acc.ID, store.CookieUpdate{
		ExpectedCookies: acc.Cookies, ExpectedExpiry: acc.CookiesExp,
		Cookies: res.Cookies, CookiesExp: res.CookiesExp, HasExpiry: true,
		OK: boolPtr(res.OK), SyncedAt: &now, Note: note + " (VM: " + vm + ")", Keepalive: true,
	})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpError(404, "account no longer exists")
	}
	return updated, nil
}

func (s *Server) syncWorkerCookies(accountID string, w *engine.Worker) (map[string]any, error) {
	live := w.ReadCookies(context.Background())
	if len(live) == 0 {
		return map[string]any{"synced": 0}, nil
	}
	acc := s.store.GetAccount(accountID)
	if acc == nil {
		return map[string]any{"synced": 0}, nil
	}
	cur := map[string]string{}
	for k, v := range acc.Cookies {
		cur[k] = v
	}
	exp := map[string]int64{}
	for k, v := range acc.CookiesExp {
		exp[k] = v
	}
	changed := 0
	newValues := map[string]string{}
	for name, info := range live {
		if info.Value != "" {
			newValues[name] = info.Value
			if cur[name] != info.Value {
				changed++
			}
			cur[name] = info.Value
		}
		if info.Expires > 0 {
			exp[name] = info.Expires
		}
	}
	now := time.Now().Unix()
	expected, expiry := w.Source()
	updated, _, err := s.store.UpdateCookies(accountID, store.CookieUpdate{
		ExpectedCookies: expected, ExpectedExpiry: expiry,
		Cookies:    cur,
		CookiesExp: exp,
		HasExpiry:  true,
		SyncedAt:   &now,
	})
	if errors.Is(err, store.ErrStaleCookies) {
		return map[string]any{"synced": 0, "stale": true}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"synced": len(newValues), "changed": changed}
	if updated != nil && updated.ExpiresAt != nil {
		out["expires_at"] = *updated.ExpiresAt
	}
	return out, nil
}

// ---------- Generation ----------

// RunGeneration owns the browser slot and account lease across account
// selection, retry and cleanup.
func (s *Server) RunGeneration(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
	if s.runGenerationFn != nil {
		return s.runGenerationFn(ctx, prompt, kind, timeout, accountID, onProgress, referenceImages, onAcquired)
	}
	return s.runGenerationReal(ctx, prompt, kind, timeout, accountID, onProgress, referenceImages, onAcquired)
}

func (s *Server) runGenerationReal(ctx context.Context, prompt, kind string, timeout time.Duration, accountID string, onProgress func(int), referenceImages []string, onAcquired func()) (*engine.GenResult, string, error) {
	if timeout < time.Second {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	w, err := s.eng.Acquire(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &engine.MuseGenerationError{Message: "timed out waiting for the browser queue; try again later"}
	}
	defer s.eng.Release(w)
	defer w.ClosePage()

	preferred := s.eng.PreferredAccount()
	if kind == "video" && accountID == "" {
		for _, task := range s.store.ListTasks(50) {
			if task.Kind != "video" || task.Status != "completed" || task.Account == "" {
				continue
			}
			candidate := s.store.GetAccount(task.Account)
			if candidate != nil && candidate.Enabled && (candidate.OK == nil || *candidate.OK) {
				preferred = candidate.ID
				break
			}
		}
	}
	remaining := time.Until(deadline)
	if remaining < time.Second {
		remaining = time.Second
	}
	cur, err := s.pool.Acquire(ctx, accountID, preferred, true, false, remaining, true)
	if err != nil {
		return nil, "", err
	}
	curID := cur.ID
	defer func() {
		if curID != "" {
			s.pool.Release(curID)
		}
	}()
	if onAcquired != nil {
		onAcquired()
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if time.Now().After(deadline) {
			lastErr = &engine.MuseGenerationError{Message: "the task deadline was reached; stopped retrying"}
			break
		}
		if attempt > 0 {
			if engine.IsRefusalError(lastErr) {
				break
			}
			oldID := curID
			s.pool.Release(oldID)
			curID = ""
			rem := time.Until(deadline)
			if rem <= 0 {
				break
			}
			alt, aerr := s.pool.Acquire(ctx, "", "", true, true, rem, true)
			if aerr != nil {
				if lastErr == nil {
					lastErr = aerr
				}
				break
			}
			cur = alt
			curID = alt.ID
			if oldID != "" && alt.ID == oldID {
				break
			}
			log.Printf("[generation failover] switched to backup account %s (%s)", alt.Label, alt.ID)
		}

		refreshed, rerr := s.renewAndPersist(ctx, curID, true, attempt > 0)
		if rerr != nil {
			lastErr = rerr
			w.ClosePage()
			continue
		}
		if refreshed != nil {
			cur = refreshed
		}
		if err := s.eng.StartContext(ctx); err != nil {
			lastErr = &engine.MuseGenerationError{Message: err.Error()}
			w.ClosePage()
			continue
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			lastErr = &engine.MuseGenerationError{Message: "the task deadline was reached; stopped retrying"}
			break
		}
		res, gerr := w.Generate(ctx, cur.Cookies, prompt, kind, remaining, cur.CookiesExp, cur.ID, onProgress, "", referenceImages)
		if gerr == nil {
			if err := s.markAccount(cur, boolPtr(true), ""); err != nil {
				return nil, "", err
			}
			if _, err := s.syncWorkerCookies(cur.ID, w); err != nil {
				return nil, "", err
			}
			s.eng.SetPreferredAccount(cur.ID)
			return res, cur.ID, nil
		}
		lastErr = gerr
		if engine.IsAuthError(gerr) {
			if err := s.markAccount(cur, boolPtr(false), gerr.Error()); err != nil {
				return nil, "", err
			}
			w.ClosePage()
			continue
		}
		if engine.IsRefusalError(gerr) {
			if err := s.markAccount(cur, boolPtr(true), truncateNote("Task refused: "+gerr.Error(), 60)); err != nil {
				return nil, "", err
			}
			w.ClosePage()
			continue
		}
		if err := s.markAccount(cur, boolPtr(true), truncateNote("Task error: "+gerr.Error(), 60)); err != nil {
			return nil, "", err
		}
		w.ClosePage()
	}

	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", &engine.MuseGenerationError{Message: "generation failed"}
}

// StreamChat runs one chat turn, emitting incremental text. It retries with a
// backup account only if nothing has been emitted yet.
func (s *Server) StreamChat(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) ([]engine.ChatFile, error) {
	if s.streamChatFn != nil {
		return s.streamChatFn(ctx, prompt, images, timeout, emit)
	}
	return s.streamChatReal(ctx, prompt, images, timeout, emit)
}

func (s *Server) streamChatReal(ctx context.Context, prompt string, images []string, timeout time.Duration, emit func(string) error) ([]engine.ChatFile, error) {
	if timeout <= 0 {
		timeout = time.Duration(s.cfg.ChatTimeout) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	w, err := s.eng.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer s.eng.Release(w)

	var cur *store.Account
	var lastErr error
	yielded := false
	curID := ""

	defer func() {
		if curID != "" {
			s.pool.Release(curID)
		}
		w.ClosePage()
	}()

	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if attempt == 0 {
			acc, aerr := s.pool.Acquire(ctx, "", s.eng.PreferredAccount(), true, false, timeout, true)
			if aerr != nil {
				return nil, aerr
			}
			cur = acc
			curID = acc.ID
		} else {
			oldID := curID
			if oldID != "" {
				s.pool.Release(oldID)
				curID = ""
			}
			alt, aerr := s.pool.Acquire(ctx, "", "", true, true, timeout, true)
			if aerr != nil {
				if lastErr == nil {
					lastErr = aerr
				}
				break
			}
			cur = alt
			curID = alt.ID
			if oldID != "" && alt.ID == oldID {
				break
			}
			log.Printf("[chat failover] switched to backup account %s (%s)", alt.Label, alt.ID)
		}

		refreshed, rerr := s.renewAndPersist(ctx, curID, true, attempt > 0)
		if rerr != nil {
			lastErr = rerr
			w.ClosePage()
			continue
		}
		if refreshed != nil {
			cur = refreshed
		}
		if err := s.eng.StartContext(ctx); err != nil {
			lastErr = err
			w.ClosePage()
			continue
		}
		emitWithFlag := func(chunk string) error {
			yielded = true
			return emit(chunk)
		}
		files, cerr := w.ChatStream(ctx, cur.Cookies, prompt, cur.CookiesExp, time.Until(deadline), cur.ID, images, emitWithFlag)
		if cerr == nil {
			if err := s.markAccount(cur, boolPtr(true), ""); err != nil {
				return nil, err
			}
			if _, err := s.syncWorkerCookies(cur.ID, w); err != nil {
				return nil, err
			}
			s.eng.SetPreferredAccount(cur.ID)
			return files, nil
		}
		lastErr = cerr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if engine.IsAuthError(cerr) {
			if err := s.markAccount(cur, boolPtr(false), cerr.Error()); err != nil {
				return nil, err
			}
			w.ClosePage()
			if yielded {
				break
			}
			continue
		}
		if err := s.markAccount(cur, boolPtr(true), truncateNote("Assistant timeout: "+cerr.Error(), 60)); err != nil {
			return nil, err
		}
		w.ClosePage()
		if yielded {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &engine.MuseGenerationError{Message: "chat generation failed"}
}

func boolPtr(v bool) *bool { return &v }

func truncateNote(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// markAccount applies health only to the credentials used by this operation.
func (s *Server) markAccount(acc *store.Account, ok *bool, note string) error {
	_, _, err := s.store.UpdateCookies(acc.ID, store.CookieUpdate{ExpectedCookies: acc.Cookies, ExpectedExpiry: acc.CookiesExp, OK: ok, Note: note, Checked: true})
	if errors.Is(err, store.ErrStaleCookies) {
		return nil
	}
	return err
}

// TryAcquire reserves an account for maintenance, including disabled accounts.
func (p *AccountPool) TryAcquire(id string) (*store.Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.busy[id] {
		return nil, false
	}
	acc := p.store.GetAccount(id)
	if acc == nil {
		return nil, false
	}
	p.busy[id] = true
	return acc, true
}
