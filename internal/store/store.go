// Package store persists the account pool and generation tasks as JSON.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// EssentialCookies are the only cookies required for a usable account.
// hatch_gw is a re-issuable routing cache and must not gate imports.
var EssentialCookies = []string{"hatch_sess", "hatch_vml"}

// VMLTTL is the observed lifetime of hatch_vml (about two days). It is used
// only to estimate an expiry when the real cookie expiry is unavailable.
const VMLTTL int64 = 2 * 86400

type Quota struct {
	Found         bool     `json:"found"`
	Plan          string   `json:"plan,omitempty"`
	WeeklyReset   string   `json:"weekly_reset,omitempty"`
	WeeklyUsedPct *float64 `json:"weekly_used_pct,omitempty"`
	ExtraLeft     string   `json:"extra_left,omitempty"`
	ExtraUsedPct  *float64 `json:"extra_used_pct,omitempty"`
	ExtraExpires  string   `json:"extra_expires,omitempty"`
	CheckedAt     int64    `json:"checked_at,omitempty"`
	Raw           string   `json:"raw,omitempty"`
}

type Account struct {
	ID            string            `json:"id"`
	Label         string            `json:"label"`
	Cookies       map[string]string `json:"cookies"`
	CookiesExp    map[string]int64  `json:"cookies_exp,omitempty"`
	ExpiresAt     *int64            `json:"expires_at"`
	ExpiryAnchor  *int64            `json:"expiry_anchor"`
	Enabled       bool              `json:"enabled"`
	CreatedAt     int64             `json:"created_at"`
	LastUsed      *int64            `json:"last_used"`
	LastKeepalive *int64            `json:"last_keepalive"`
	UseCount      int64             `json:"use_count"`
	OK            *bool             `json:"ok"`
	Note          string            `json:"note"`
	SyncedAt      *int64            `json:"synced_at"`
	CheckedAt     *int64            `json:"checked_at,omitempty"`
	Quota         *Quota            `json:"quota,omitempty"`
}

type AccountView struct {
	ID               string           `json:"id"`
	Label            string           `json:"label"`
	CookiesExp       map[string]int64 `json:"cookies_exp,omitempty"`
	ExpiresAt        *int64           `json:"expires_at"`
	ExpiryAnchor     *int64           `json:"expiry_anchor"`
	Enabled          bool             `json:"enabled"`
	CreatedAt        int64            `json:"created_at"`
	LastUsed         *int64           `json:"last_used"`
	LastKeepalive    *int64           `json:"last_keepalive"`
	UseCount         int64            `json:"use_count"`
	OK               *bool            `json:"ok"`
	Note             string           `json:"note"`
	SyncedAt         *int64           `json:"synced_at"`
	CheckedAt        *int64           `json:"checked_at,omitempty"`
	Quota            *Quota           `json:"quota,omitempty"`
	CookieCount      int              `json:"cookie_count"`
	EssentialOK      bool             `json:"essential_ok"`
	ExpiresEstimated bool             `json:"expires_estimated"`
}

type Stats struct {
	Total    int `json:"total"`
	Enabled  int `json:"enabled"`
	Disabled int `json:"disabled"`
	Healthy  int `json:"healthy"`
	Error    int `json:"error"`
	Expiring int `json:"expiring"`
	Expired  int `json:"expired"`
}

type GenerationResult struct {
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Bytes    int64  `json:"bytes,omitempty"`
	Kind     string `json:"kind,omitempty"`
	URL      string `json:"url,omitempty"`
}

type Task struct {
	ID               string            `json:"id"`
	Object           string            `json:"object"`
	Kind             string            `json:"kind"`
	Prompt           string            `json:"prompt"`
	APIPrompt        string            `json:"api_prompt,omitempty"`
	Status           string            `json:"status"`
	CreatedAt        int64             `json:"created_at"`
	UpdatedAt        int64             `json:"updated_at"`
	Result           *GenerationResult `json:"result"`
	Video            map[string]string `json:"video,omitempty"`
	URL              string            `json:"url,omitempty"`
	Error            string            `json:"error,omitempty"`
	Progress         int               `json:"progress"`
	Elapsed          float64           `json:"elapsed,omitempty"`
	Account          string            `json:"account,omitempty"`
	Size             string            `json:"size,omitempty"`
	ResponseFormat   string            `json:"response_format,omitempty"`
	ImageRequestKey  string            `json:"image_request_key,omitempty"`
	ImageRequestHash string            `json:"image_request_hash,omitempty"`
}

type Store struct {
	mu sync.Mutex

	accounts      []*Account
	tasks         map[string]*Task
	savedAccounts []*Account
	savedTasks    map[string]*Task

	accountsFile string
	tasksFile    string

	stickyID    string
	stickyCount int
}

func New(accountsFile, tasksFile string) (*Store, error) {
	s := &Store{
		accountsFile: accountsFile,
		tasksFile:    tasksFile,
		tasks:        map[string]*Task{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.savedAccounts = copyAccounts(s.accounts)
	s.savedTasks = copyTasks(s.tasks)
	return s, nil
}

func (s *Store) load() error {
	read := func(path string, value any) error {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := json.Unmarshal(data, value); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		return nil
	}
	if err := read(s.accountsFile, &s.accounts); err != nil {
		return err
	}
	for _, a := range s.accounts {
		if a == nil {
			return errors.New("null account in account store")
		}
		if a.Cookies == nil {
			a.Cookies = map[string]string{}
		}
		if a.CookiesExp == nil {
			a.CookiesExp = map[string]int64{}
		}
		if vml := a.CookiesExp["hatch_vml"]; vml > 0 {
			a.ExpiresAt = &vml
		}
	}
	if err := read(s.tasksFile, &s.tasks); err != nil {
		return err
	}
	if s.tasks == nil {
		s.tasks = map[string]*Task{}
	}
	for _, t := range s.tasks {
		if t == nil {
			return errors.New("null task in task store")
		}
	}
	return nil
}
func copyAccounts(accounts []*Account) []*Account {
	out := make([]*Account, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.copy())
	}
	return out
}
func copyTasks(tasks map[string]*Task) map[string]*Task {
	out := make(map[string]*Task, len(tasks))
	for id, t := range tasks {
		out[id] = t.copy()
	}
	return out
}
func (s *Store) saveAccountsLocked() error {
	if err := writeJSONAtomic(s.accountsFile, s.accounts); err != nil {
		s.accounts = copyAccounts(s.savedAccounts)
		return fmt.Errorf("save accounts: %w", err)
	}
	s.savedAccounts = copyAccounts(s.accounts)
	return nil
}
func (s *Store) saveTasksLocked() error {
	if err := writeJSONAtomic(s.tasksFile, s.tasks); err != nil {
		s.tasks = copyTasks(s.savedTasks)
		return fmt.Errorf("save tasks: %w", err)
	}
	s.savedTasks = copyTasks(s.tasks)
	return nil
}

func writeJSONAtomic(path string, value any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(value); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(buf.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func Positive(v int64) bool { return v > 0 }

// MinExpiry returns the earliest positive expiry among the essential cookies,
// falling back to any cookie.
func MinExpiry(exp map[string]int64) *int64 {
	var vals []int64
	for _, name := range EssentialCookies {
		if v := exp[name]; v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		for _, v := range exp {
			if v > 0 {
				vals = append(vals, v)
			}
		}
	}
	if len(vals) == 0 {
		return nil
	}
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return &min
}

// AccountExpiry implements the documented priority:
//  1. the real hatch_vml expiry;
//  2. base timestamp + VMLTTL (when an anchor/created time exists);
//  3. the earliest essential cookie expiry.
func AccountExpiry(exp map[string]int64, base *int64) *int64 {
	if v := exp["hatch_vml"]; v > 0 {
		return &v
	}
	if base != nil && *base > 0 {
		v := *base + VMLTTL
		return &v
	}
	return MinExpiry(exp)
}

func (a *Account) essentialOK() bool {
	for _, name := range EssentialCookies {
		if a.Cookies[name] == "" {
			return false
		}
	}
	return true
}

func (a *Account) View() AccountView {
	exp := a.CookiesExp
	if exp == nil {
		exp = map[string]int64{}
	}
	expires := a.ExpiresAt
	if v := exp["hatch_vml"]; v > 0 {
		vv := v
		expires = &vv
	}
	return AccountView{
		ID:               a.ID,
		Label:            a.Label,
		CookiesExp:       cloneIntMap(exp),
		ExpiresAt:        cloneIntPtr(expires),
		ExpiryAnchor:     cloneIntPtr(a.ExpiryAnchor),
		Enabled:          a.Enabled,
		CreatedAt:        a.CreatedAt,
		LastUsed:         cloneIntPtr(a.LastUsed),
		LastKeepalive:    cloneIntPtr(a.LastKeepalive),
		UseCount:         a.UseCount,
		OK:               cloneBoolPtr(a.OK),
		Note:             a.Note,
		SyncedAt:         cloneIntPtr(a.SyncedAt),
		CheckedAt:        cloneIntPtr(a.CheckedAt),
		Quota:            cloneQuota(a.Quota),
		CookieCount:      len(a.Cookies),
		EssentialOK:      a.essentialOK(),
		ExpiresEstimated: !Positive(exp["hatch_vml"]),
	}
}

func (a *Account) copy() *Account {
	if a == nil {
		return nil
	}
	return &Account{
		ID:            a.ID,
		Label:         a.Label,
		Cookies:       cloneStringMap(a.Cookies),
		CookiesExp:    cloneIntMap(a.CookiesExp),
		ExpiresAt:     cloneIntPtr(a.ExpiresAt),
		ExpiryAnchor:  cloneIntPtr(a.ExpiryAnchor),
		Enabled:       a.Enabled,
		CreatedAt:     a.CreatedAt,
		LastUsed:      cloneIntPtr(a.LastUsed),
		LastKeepalive: cloneIntPtr(a.LastKeepalive),
		UseCount:      a.UseCount,
		OK:            cloneBoolPtr(a.OK),
		Note:          a.Note,
		SyncedAt:      cloneIntPtr(a.SyncedAt),
		CheckedAt:     cloneIntPtr(a.CheckedAt),
		Quota:         cloneQuota(a.Quota),
	}
}

func (t *Task) copy() *Task {
	if t == nil {
		return nil
	}
	c := *t
	if t.Result != nil {
		r := *t.Result
		c.Result = &r
	}
	if t.Video != nil {
		c.Video = map[string]string{}
		for k, v := range t.Video {
			c.Video[k] = v
		}
	}
	return &c
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneIntMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneIntPtr(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func cloneQuota(in *Quota) *Quota {
	if in == nil {
		return nil
	}
	q := *in
	if in.WeeklyUsedPct != nil {
		v := *in.WeeklyUsedPct
		q.WeeklyUsedPct = &v
	}
	if in.ExtraUsedPct != nil {
		v := *in.ExtraUsedPct
		q.ExtraUsedPct = &v
	}
	return &q
}

func randomID(prefix string, bytesCount int) string {
	b := make([]byte, bytesCount)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(b)
}

// ---------- Accounts ----------

func (s *Store) AddAccount(cookies map[string]string, label string, cookiesExp map[string]int64) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	exp := map[string]int64{}
	for k, v := range cookiesExp {
		if v > 0 {
			exp[k] = v
		}
	}
	id := randomID("", 6)
	acc := &Account{
		ID:         id,
		Label:      label,
		Cookies:    cloneStringMap(cookies),
		CookiesExp: exp,
		Enabled:    true,
		CreatedAt:  now,
		Note:       "",
	}
	if acc.Label == "" {
		acc.Label = id
	}
	acc.ExpiresAt = AccountExpiry(exp, &now)
	s.accounts = append(s.accounts, acc)
	if err := s.saveAccountsLocked(); err != nil {
		return nil, err
	}
	return acc.copy(), nil
}

func (s *Store) ListAccounts() []AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountView, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a.View())
	}
	return out
}

func (s *Store) GetAccount(id string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return a.copy()
		}
	}
	return nil
}

func (s *Store) DeleteAccount(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			if err := s.saveAccountsLocked(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) UpdateLabelEnabled(id string, label *string, enabled *bool) (*Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		if label != nil {
			a.Label = *label
		}
		if enabled != nil {
			a.Enabled = *enabled
		}
		if err := s.saveAccountsLocked(); err != nil {
			return nil, false, err
		}
		return a.copy(), true, nil
	}
	return nil, false, nil
}

// CookieUpdate describes an account cookie refresh. When HasExpiry is false
// the stored expiry data is left untouched.
var ErrStaleCookies = errors.New("account cookies changed during operation")
var ErrActiveTask = errors.New("task is still active")

type CookieUpdate struct {
	Checked         bool
	Keepalive       bool
	ExpectedCookies map[string]string
	ExpectedExpiry  map[string]int64
	Cookies         map[string]string
	CookiesExp      map[string]int64
	HasExpiry       bool
	ResetAnchor     bool
	Note            string
	OK              *bool
	SyncedAt        *int64
	Quota           *Quota
}

func (s *Store) UpdateCookies(id string, u CookieUpdate) (*Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		if u.ExpectedCookies != nil && (!maps.Equal(a.Cookies, u.ExpectedCookies) || !maps.Equal(a.CookiesExp, u.ExpectedExpiry)) {
			return a.copy(), true, ErrStaleCookies
		}
		if u.Cookies != nil {
			a.Cookies = cloneStringMap(u.Cookies)
		}
		if u.HasExpiry {
			exp := map[string]int64{}
			for k, v := range u.CookiesExp {
				if v > 0 {
					exp[k] = v
				}
			}
			a.CookiesExp = exp
			now := time.Now().Unix()
			if exp["hatch_vml"] > 0 {
				a.ExpiryAnchor = nil
			} else if u.ResetAnchor || a.ExpiryAnchor == nil {
				a.ExpiryAnchor = &now
			}
			base := a.ExpiryAnchor
			if base == nil {
				base = &a.CreatedAt
			}
			a.ExpiresAt = AccountExpiry(exp, base)
		}
		if u.Note != "" {
			a.Note = truncate(u.Note, 300)
		}
		if u.OK != nil {
			a.OK = cloneBoolPtr(u.OK)
		}
		if u.Checked {
			now := time.Now().Unix()
			a.CheckedAt = &now
		}
		if u.Keepalive && u.OK != nil && *u.OK {
			now := time.Now().Unix()
			a.LastKeepalive = &now
		}
		if u.SyncedAt != nil {
			a.SyncedAt = cloneIntPtr(u.SyncedAt)
		}
		if u.Quota != nil {
			a.Quota = cloneQuota(u.Quota)
		}
		if err := s.saveAccountsLocked(); err != nil {
			return nil, false, err
		}
		return a.copy(), true, nil
	}
	return nil, false, nil
}

func (s *Store) SetQuota(id string, quota *Quota) (*Account, bool, error) {
	return s.UpdateCookies(id, CookieUpdate{Quota: quota})
}

func (s *Store) Mark(id string, ok bool, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			v := ok
			a.OK = &v
			a.Note = truncate(note, 300)
			now := time.Now().Unix()
			a.CheckedAt = &now
		}
	}
	if err := s.saveAccountsLocked(); err != nil {
		return err
	}
	return nil
}

// TouchKeepalive records a check. ok == nil preserves the previous health
// state. A successful check never fabricates a longer expiry.

func (s *Store) TouchKeepalive(id string, ok *bool, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		now := time.Now().Unix()
		if ok != nil {
			a.OK = cloneBoolPtr(ok)
		}
		if ok != nil && *ok {
			a.LastKeepalive = &now
			base := a.ExpiryAnchor
			if base == nil {
				base = &a.CreatedAt
			}
			a.ExpiresAt = AccountExpiry(a.CookiesExp, base)
		}
		if note != "" {
			a.Note = truncate(note, 300)
		}
		a.CheckedAt = &now
	}
	if err := s.saveAccountsLocked(); err != nil {
		return err
	}
	return nil
}

func (s *Store) PickAccount(preferredID string, rotate bool, forceRotate bool, excludeIDs map[string]bool, recordUse bool) (*Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	live := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		if a.Enabled && len(a.Cookies) > 0 {
			live = append(live, a)
		}
	}
	if len(live) == 0 {
		return nil, nil
	}

	blocked := map[string]bool{}
	for id := range excludeIDs {
		blocked[id] = true
	}
	if len(blocked) > 0 {
		available := make([]*Account, 0, len(live))
		for _, a := range live {
			if !blocked[a.ID] {
				available = append(available, a)
			}
		}
		if len(available) > 0 {
			live = available
		} else if excludeIDs != nil {
			return nil, nil
		}
	}

	if preferredID != "" && !forceRotate {
		for _, a := range live {
			if a.ID != preferredID || (a.OK != nil && !*a.OK) {
				continue
			}
			stickyN := 0
			if s.stickyID == preferredID {
				stickyN = s.stickyCount
			}
			if !rotate || stickyN < 15 {
				s.stickyID = preferredID
				s.stickyCount = stickyN + 1
				if recordUse {
					now := time.Now().Unix()
					a.LastUsed = &now
					a.UseCount++
					if err := s.saveAccountsLocked(); err != nil {
						return nil, err
					}
				}
				return a.copy(), nil
			}
		}
	}

	healthy := make([]*Account, 0, len(live))
	for _, a := range live {
		if a.OK == nil || *a.OK {
			healthy = append(healthy, a)
		}
	}
	candidates := healthy
	if len(candidates) == 0 {
		candidates = live
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		li, lj := int64(0), int64(0)
		if candidates[i].LastUsed != nil {
			li = *candidates[i].LastUsed
		}
		if candidates[j].LastUsed != nil {
			lj = *candidates[j].LastUsed
		}
		if li != lj {
			return li < lj
		}
		return candidates[i].UseCount < candidates[j].UseCount
	})
	acc := candidates[0]
	s.stickyID = acc.ID
	s.stickyCount = 1
	if recordUse {
		now := time.Now().Unix()
		acc.LastUsed = &now
		acc.UseCount++
		if err := s.saveAccountsLocked(); err != nil {
			return nil, err
		}
	}
	return acc.copy(), nil
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	st := Stats{Total: len(s.accounts)}
	for _, a := range s.accounts {
		if a.Enabled {
			st.Enabled++
			if a.OK != nil && *a.OK {
				st.Healthy++
			}
			if a.OK != nil && !*a.OK {
				st.Error++
			}
			if a.ExpiresAt != nil {
				if *a.ExpiresAt <= now {
					st.Expired++
				}
				if *a.ExpiresAt-now < 3*86400 {
					st.Expiring++
				}
			}
		}
	}
	st.Disabled = st.Total - st.Enabled
	return st
}

func (s *Store) AllEnabledForKeepalive(includeDisabled bool) []AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []AccountView{}
	for _, a := range s.accounts {
		if includeDisabled || a.Enabled {
			out = append(out, a.View())
		}
	}
	return out
}

// ---------- Tasks ----------

func (s *Store) CreateTask(kind, prompt string, init ...func(*Task)) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	id := randomID("task_", 10)
	t := &Task{
		ID:        id,
		Object:    "task",
		Kind:      kind,
		Prompt:    prompt,
		Status:    "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, apply := range init {
		apply(t)
	}
	s.tasks[id] = t
	if err := s.saveTasksLocked(); err != nil {
		return nil, err
	}
	return t.copy(), nil
}

// UpdateTask applies a mutation and persists it. It returns nil when missing.
func (s *Store) UpdateTask(id string, apply func(*Task)) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return nil, nil
	}
	apply(t)
	t.UpdatedAt = time.Now().Unix()
	if err := s.saveTasksLocked(); err != nil {
		return nil, err
	}
	return t.copy(), nil
}

func (s *Store) GetTask(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tasks[id].copy()
}

func (s *Store) ListTasks(limit int) []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		items = append(items, t.copy())
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt > items[j].CreatedAt })
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

func (s *Store) DeleteTask(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[id]; !ok {
		return false, nil
	}
	if s.tasks[id].Active() {
		return false, ErrActiveTask
	}
	delete(s.tasks, id)
	if err := s.saveTasksLocked(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ClearTasks(keep int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]*Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		items = append(items, t)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].CreatedAt > items[j].CreatedAt })
	keepIDs := map[string]bool{}
	for i, t := range items {
		if i < keep {
			keepIDs[t.ID] = true
		}
	}
	removed := 0
	for id := range s.tasks {
		if !keepIDs[id] && !s.tasks[id].Active() {
			delete(s.tasks, id)
			removed++
		}
	}
	if err := s.saveTasksLocked(); err != nil {
		return 0, err
	}
	return removed, nil
}

func (t *Task) Active() bool { return t.Status == "queued" || t.Status == "processing" }

func (s *Store) CountActiveTasks() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tasks {
		if t.Active() {
			n++
		}
	}
	return n
}

func (s *Store) FindByImageRequestKey(keyHash string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ImageRequestKey == keyHash {
			return t.copy()
		}
	}
	return nil
}

func (s *Store) MarkInterruptedTasks() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, t := range s.tasks {
		if (t.Kind == "image" || t.Kind == "video") && (t.Status == "queued" || t.Status == "processing") {
			t.Status = "failed"
			t.Error = "Service restart interrupted this task; please submit it again"
			t.UpdatedAt = time.Now().Unix()
			n++
		}
	}
	if n > 0 {
		if err := s.saveTasksLocked(); err != nil {
			return 0, err
		}
	}
	return n, nil
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}
