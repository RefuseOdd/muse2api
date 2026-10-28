package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPersistenceRollback(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(map[string]string{"hatch_sess": "old"}, "original", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := s.accountsFile
	blocked := filepath.Join(t.TempDir(), "blocked")
	os.WriteFile(blocked, []byte("file"), 0600)
	s.accountsFile = filepath.Join(blocked, "accounts.json")
	if got, err := s.AddAccount(map[string]string{"hatch_sess": "new"}, "new", nil); err == nil || got != nil {
		t.Fatal("failed add returned success")
	}
	if _, _, err := s.UpdateCookies(acc.ID, CookieUpdate{Cookies: map[string]string{"hatch_sess": "new"}}); err == nil {
		t.Fatal("failed update returned success")
	}
	if got := s.GetAccount(acc.ID); got.Cookies["hatch_sess"] != "old" || len(s.ListAccounts()) != 1 {
		t.Fatal("failed save changed memory")
	}
	s.accountsFile = original
	reopened, err := New(original, s.tasksFile)
	if err != nil || reopened.GetAccount(acc.ID).Cookies["hatch_sess"] != "old" {
		t.Fatal("failed save changed disk")
	}
}

func TestInvalidStoreFailsStartup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "accounts.json")
	os.WriteFile(file, []byte("broken JSON"), 0600)
	if _, err := New(file, filepath.Join(dir, "tasks.json")); err == nil {
		t.Fatal("invalid store silently treated as empty")
	}
}

func TestCookieSnapshotRejectsStaleWrite(t *testing.T) {
	s := newTestStore(t)
	acc, err := s.AddAccount(map[string]string{"hatch_sess": "old"}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpdateCookies(acc.ID, CookieUpdate{Cookies: map[string]string{"hatch_sess": "import"}}); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.UpdateCookies(acc.ID, CookieUpdate{ExpectedCookies: acc.Cookies, ExpectedExpiry: acc.CookiesExp, Cookies: map[string]string{"hatch_sess": "late"}})
	if !errors.Is(err, ErrStaleCookies) || s.GetAccount(acc.ID).Cookies["hatch_sess"] != "import" {
		t.Fatal("stale cookies accepted")
	}
}
