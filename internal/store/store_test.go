package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "accounts.json"), filepath.Join(dir, "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAccountExpiryPriority(t *testing.T) {
	exp := map[string]int64{"hatch_sess": 5000, "hatch_vml": 3000, "other": 100}
	if got := AccountExpiry(exp, nil); got == nil || *got != 3000 {
		t.Fatalf("expected hatch_vml expiry 3000, got %v", got)
	}
	base := int64(10_000)
	got := AccountExpiry(map[string]int64{"hatch_sess": 50_000}, &base)
	if got == nil || *got != base+VMLTTL {
		t.Fatalf("expected estimated expiry %d, got %v", base+VMLTTL, got)
	}
	got = AccountExpiry(map[string]int64{"hatch_sess": 20_000, "foo": 9000}, nil)
	if got == nil || *got != 20_000 {
		t.Fatalf("expected earliest essential expiry 20000, got %v", got)
	}
	got = AccountExpiry(map[string]int64{"foo": 8000, "bar": 9000}, nil)
	if got == nil || *got != 8000 {
		t.Fatalf("expected earliest fallback expiry 8000, got %v", got)
	}
}

func TestAddListAndExpiryEstimate(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(map[string]string{"hatch_sess": "s", "hatch_vml": "v"}, "", map[string]int64{"hatch_vml": 123456})
	if acc.Label != acc.ID {
		t.Fatalf("label should default to id")
	}
	views := s.ListAccounts()
	if len(views) != 1 {
		t.Fatalf("expected one account")
	}
	v := views[0]
	if !v.EssentialOK || v.CookieCount != 2 || !v.Enabled {
		t.Fatalf("unexpected account view: %+v", v)
	}
	if v.ExpiresAt == nil || *v.ExpiresAt != 123456 || v.ExpiresEstimated {
		t.Fatalf("unexpected expiry: %+v", v)
	}

	acc2, _ := s.AddAccount(map[string]string{"hatch_sess": "s"}, "second", nil)
	if acc2.ExpiresAt == nil {
		t.Fatalf("expected estimated expiry")
	}
	views = s.ListAccounts()
	var second AccountView
	for _, view := range views {
		if view.ID == acc2.ID {
			second = view
		}
	}
	if !second.ExpiresEstimated {
		t.Fatalf("expected estimated expiry flag")
	}
}

func TestTouchKeepaliveDoesNotFabricateExpiry(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(map[string]string{"hatch_sess": "s"}, "", nil)
	before := *acc.ExpiresAt
	time.Sleep(5 * time.Millisecond)
	ok := true
	s.TouchKeepalive(acc.ID, &ok, "checked")
	after := s.GetAccount(acc.ID)
	if after.ExpiresAt == nil || *after.ExpiresAt != before {
		t.Fatalf("keepalive must not push estimated expiry: before=%d after=%v", before, after.ExpiresAt)
	}
	if after.LastKeepalive == nil || after.OK == nil || !*after.OK {
		t.Fatalf("expected keepalive timestamp and healthy state")
	}
}

func TestPickAccountExclusionAndUsage(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.AddAccount(map[string]string{"hatch_sess": "a"}, "a", nil)
	b, _ := s.AddAccount(map[string]string{"hatch_sess": "b"}, "b", nil)

	got, _ := s.PickAccount(a.ID, false, false, nil, false)
	if got == nil || got.ID != a.ID {
		t.Fatalf("preferred account should win")
	}
	if got.UseCount != 0 || got.LastUsed != nil {
		t.Fatalf("recordUse=false must not change usage counters")
	}
	got, _ = s.PickAccount("", true, false, map[string]bool{a.ID: true}, true)
	if got == nil || got.ID != b.ID {
		t.Fatalf("busy account should be excluded")
	}
	got, _ = s.PickAccount("", true, false, map[string]bool{a.ID: true, b.ID: true}, true)
	if got != nil {
		t.Fatalf("expected nil when every account is busy")
	}
	// With no exclusions it rotates back to the least recently used account.
	got, _ = s.PickAccount("", true, false, nil, true)
	if got == nil {
		t.Fatalf("expected an account")
	}
}

func TestCookieUpdateAnchorRules(t *testing.T) {
	s := newTestStore(t)
	acc, _ := s.AddAccount(map[string]string{"hatch_sess": "s"}, "", nil)
	updated, ok, _ := s.UpdateCookies(acc.ID, CookieUpdate{
		Cookies:    map[string]string{"hatch_sess": "new", "hatch_vml": "v"},
		CookiesExp: map[string]int64{"hatch_vml": 900000},
		HasExpiry:  true,
	})
	if !ok || updated.Cookies["hatch_vml"] != "v" {
		t.Fatalf("cookie update failed")
	}
	if updated.ExpiresAt == nil || *updated.ExpiresAt != 900000 || updated.ExpiryAnchor != nil {
		t.Fatalf("real hatch_vml expiry must clear the anchor: %+v", updated)
	}
}

func TestInterruptedTasksAndClear(t *testing.T) {
	s := newTestStore(t)
	t1, _ := s.CreateTask("image", "one")
	t2, _ := s.CreateTask("video", "two")
	s.UpdateTask(t1.ID, func(task *Task) { task.Status = "processing" })
	s.UpdateTask(t2.ID, func(task *Task) { task.Status = "completed" })
	if n, _ := s.MarkInterruptedTasks(); n != 1 {
		t.Fatalf("expected one interrupted task, got %d", n)
	}
	if got := s.GetTask(t1.ID); got.Status != "failed" || got.Error == "" {
		t.Fatalf("interrupted task was not marked failed: %+v", got)
	}
	if got := s.GetTask(t2.ID); got.Status != "completed" {
		t.Fatalf("completed task should be untouched")
	}
	if removed, _ := s.ClearTasks(0); removed != 2 {
		t.Fatalf("expected two removed tasks, got %d", removed)
	}
	if len(s.ListTasks(10)) != 0 {
		t.Fatalf("expected empty task list")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	accounts := filepath.Join(dir, "accounts.json")
	tasks := filepath.Join(dir, "tasks.json")
	s, _ := New(accounts, tasks)
	acc, _ := s.AddAccount(map[string]string{"hatch_sess": "s", "hatch_vml": "v"}, "persisted", map[string]int64{"hatch_vml": 42})
	s.CreateTask("image", "prompt")
	if _, err := os.Stat(accounts); err != nil {
		t.Fatalf("accounts file missing: %v", err)
	}
	reloaded, _ := New(accounts, tasks)
	got := reloaded.GetAccount(acc.ID)
	if got == nil || got.Cookies["hatch_vml"] != "v" || got.Label != "persisted" {
		t.Fatalf("account did not round-trip: %+v", got)
	}
	if len(reloaded.ListTasks(10)) != 1 {
		t.Fatalf("task did not round-trip")
	}
}
