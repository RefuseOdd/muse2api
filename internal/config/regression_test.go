package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPersistedKeyWithBlankEnvironment(t *testing.T) {
	base := t.TempDir()
	env := filepath.Join(base, "data", ".env")
	t.Setenv("MUSE2API_HOME", base)
	t.Setenv("MUSE2API_ENV_FILE", env)
	t.Setenv("MUSE2API_KEY", "")
	first := Load()
	if first.ExternalKey {
		t.Fatal("blank key treated as environment-owned")
	}
	if err := first.PersistEnv("MUSE2API_KEY", "persisted"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MUSE2API_KEY", "")
	second := Load()
	if second.APIKey != "persisted" || second.ExternalKey {
		t.Fatal("restart lost persisted key")
	}
	t.Setenv("MUSE2API_KEY", "external")
	third := Load()
	if third.APIKey != "external" || !third.ExternalKey {
		t.Fatal("explicit deployment key not respected")
	}
	if _, err := os.Stat(env); err != nil {
		t.Fatal(err)
	}
}
