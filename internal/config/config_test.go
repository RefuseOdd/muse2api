package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDotEnvAndConfiguration(t *testing.T) {
	dir := t.TempDir()
	envContent := strings.Join([]string{
		"# comment",
		"MUSE2API_KEY=from_file",
		"MUSE2API_PORT=19000",
		"MUSE2API_PUBLIC_BASE=https://example.test/",
		"MUSE2API_REF_IMAGE_LOCAL_FILES=1",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHome, hadHome := os.LookupEnv("MUSE2API_HOME")
	oldKey, hadKey := os.LookupEnv("MUSE2API_KEY")
	oldPort, hadPort := os.LookupEnv("MUSE2API_PORT")
	oldPublic, hadPublic := os.LookupEnv("MUSE2API_PUBLIC_BASE")
	oldLocal, hadLocal := os.LookupEnv("MUSE2API_REF_IMAGE_LOCAL_FILES")
	defer func() {
		restoreEnv("MUSE2API_HOME", oldHome, hadHome)
		restoreEnv("MUSE2API_KEY", oldKey, hadKey)
		restoreEnv("MUSE2API_PORT", oldPort, hadPort)
		restoreEnv("MUSE2API_PUBLIC_BASE", oldPublic, hadPublic)
		restoreEnv("MUSE2API_REF_IMAGE_LOCAL_FILES", oldLocal, hadLocal)
	}()
	for _, key := range []string{"MUSE2API_KEY", "MUSE2API_PORT", "MUSE2API_PUBLIC_BASE", "MUSE2API_REF_IMAGE_LOCAL_FILES"} {
		_ = os.Unsetenv(key)
	}
	if err := os.Setenv("MUSE2API_HOME", dir); err != nil {
		t.Fatal(err)
	}

	cfg := Load()
	if cfg.BaseDir != dir {
		t.Fatalf("base dir = %q", cfg.BaseDir)
	}
	if cfg.APIKey != "from_file" || cfg.Port != 19000 {
		t.Fatalf("unexpected config: key=%q port=%d", cfg.APIKey, cfg.Port)
	}
	if cfg.PublicURL != "https://example.test" {
		t.Fatalf("public URL should be trimmed: %q", cfg.PublicURL)
	}
	if !cfg.RefImageLocalFiles {
		t.Fatalf("boolean env was not parsed")
	}
	if cfg.BaseURL() != "https://example.test/v1" {
		t.Fatalf("unexpected base URL: %q", cfg.BaseURL())
	}
	if cfg.MediaURL("x.webp") != "https://example.test/v1/media/x.webp" {
		t.Fatalf("unexpected media URL: %q", cfg.MediaURL("x.webp"))
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.DataDir(), cfg.MediaDir(), cfg.DownloadDir(), cfg.ProfileDir()} {
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			t.Fatalf("directory missing: %s (%v)", path, err)
		}
	}
}

func TestPersistEnvPreservesOtherLines(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{BaseDir: dir}
	initial := "# keep me\nMUSE2API_PORT=18610\nMUSE2API_KEY=old\n"
	if err := os.WriteFile(cfg.EnvFile(), []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cfg.PersistEnv("MUSE2API_KEY", "new-key"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.PersistEnv("GITHUB_TOKEN", "token"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.EnvFile())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"# keep me", "MUSE2API_PORT=18610", "MUSE2API_KEY=new-key", "GITHUB_TOKEN=token"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in .env:\n%s", want, text)
		}
	}
	if strings.Contains(text, "MUSE2API_KEY=old") {
		t.Fatalf("old value was not replaced")
	}
}

func restoreEnv(key, value string, existed bool) {
	if existed {
		_ = os.Setenv(key, value)
	} else {
		_ = os.Unsetenv(key)
	}
}
