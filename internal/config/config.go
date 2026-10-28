// Package config loads muse2api settings from environment variables and .env.
package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const DefaultVersion = "2.0.0"

// Config holds every runtime setting. Values are resolved once at startup.
type Config struct {
	envMu       sync.Mutex
	EnvPath     string
	ExternalKey bool
	BaseDir     string
	Host        string
	Port        int
	PublicURL   string

	Chromium  string
	CDPPort   int
	HomeDir   string
	ExtraPath string
	SiteURL   string
	LoginWait int

	ProfileRoot string

	APIKey      string
	CORSOrigins string

	ImageTimeout   int
	VideoTimeout   int
	ChatTimeout    int
	MaxConcurrency int

	RefImageLocalFiles        bool
	RefImagePrivateFetch      bool
	ToolProtocol              bool
	ChatFiles                 bool
	KeepaliveDisabledAccounts bool

	// RepoURL is the GitHub repository used by the update panel.
	// It is optional: leave empty to disable remote update checks.
	RepoURL        string
	RepoBranch     string
	GitHubToken    string
	RebuildCommand string

	Version string
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	dir := filepath.Dir(exe)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

// Load reads .env from the configured home directory (without overriding the
// process environment) and returns a fully resolved Config.
func Load() *Config {
	base := strings.TrimSpace(os.Getenv("MUSE2API_HOME"))
	if base == "" {
		base = executableDir()
	}
	base, _ = filepath.Abs(base)
	external := strings.TrimSpace(os.Getenv("MUSE2API_KEY")) != ""
	envPath := strings.TrimSpace(os.Getenv("MUSE2API_ENV_FILE"))
	if envPath == "" {
		envPath = filepath.Join(base, ".env")
	}
	loadDotEnv(envPath)
	cfg := FromEnv(base)
	cfg.ExternalKey = external
	return cfg
}

// FromEnv builds a Config from the current environment using base as home.
func FromEnv(base string) *Config {
	get := func(key, def string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return def
	}
	getInt := func(key string, def int) int {
		v := strings.TrimSpace(get(key, ""))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return def
		}
		return n
	}
	getBool := func(key string, def bool) bool {
		v := strings.TrimSpace(get(key, ""))
		if v == "" {
			return def
		}
		return v != "0" && !strings.EqualFold(v, "false")
	}

	home, _ := os.UserHomeDir()
	cfg := &Config{
		BaseDir:                   base,
		EnvPath:                   get("MUSE2API_ENV_FILE", filepath.Join(base, ".env")),
		ExternalKey:               strings.TrimSpace(os.Getenv("MUSE2API_KEY")) != "",
		Host:                      get("MUSE2API_HOST", "0.0.0.0"),
		Port:                      getInt("MUSE2API_PORT", 18610),
		PublicURL:                 get("MUSE2API_PUBLIC_BASE", ""),
		Chromium:                  DetectChromium(get("MUSE2API_CHROMIUM", "")),
		CDPPort:                   getInt("MUSE2API_CDP_PORT", 19210),
		HomeDir:                   get("MUSE2API_HOME_DIR", home),
		ExtraPath:                 get("MUSE2API_EXTRA_PATH", "/snap/bin"),
		SiteURL:                   get("MUSE2API_SITE", "https://muse.ai/"),
		LoginWait:                 getInt("MUSE2API_LOGIN_WAIT", 60),
		ProfileRoot:               get("MUSE2API_PROFILE_ROOT", filepath.Join(home, "muse2api-profiles")),
		APIKey:                    strings.TrimSpace(get("MUSE2API_KEY", "")),
		CORSOrigins:               get("MUSE2API_CORS_ORIGINS", "https://muse.ai,https://www.muse.ai"),
		ImageTimeout:              getInt("MUSE2API_IMAGE_TIMEOUT", 240),
		VideoTimeout:              getInt("MUSE2API_VIDEO_TIMEOUT", 600),
		ChatTimeout:               getInt("MUSE2API_CHAT_TIMEOUT", 300),
		MaxConcurrency:            max(1, getInt("MUSE2API_CONCURRENCY", 2)),
		RefImageLocalFiles:        getBool("MUSE2API_REF_IMAGE_LOCAL_FILES", false),
		RefImagePrivateFetch:      getBool("MUSE2API_REF_IMAGE_PRIVATE_FETCH", false),
		ToolProtocol:              getBool("MUSE2API_TOOL_PROTOCOL", false),
		ChatFiles:                 getBool("MUSE2API_CHAT_FILES", true),
		KeepaliveDisabledAccounts: getBool("MUSE2API_KEEPALIVE_DISABLED_ACCOUNTS", false),
		RepoURL:                   strings.TrimSpace(get("MUSE2API_REPO", "")),
		RepoBranch:                get("MUSE2API_REPO_BRANCH", "main"),
		GitHubToken:               strings.TrimSpace(get("GITHUB_TOKEN", "")),
		RebuildCommand:            strings.TrimSpace(get("MUSE2API_REBUILD_CMD", "")),
		Version:                   DefaultVersion,
	}
	if cfg.LoginWait < 0 {
		cfg.LoginWait = 0
	}
	if cfg.ImageTimeout < 1 {
		cfg.ImageTimeout = 240
	}
	if cfg.VideoTimeout < 1 {
		cfg.VideoTimeout = 600
	}
	if cfg.ChatTimeout < 1 {
		cfg.ChatTimeout = 300
	}
	cfg.PublicURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/")
	return cfg
}

func (c *Config) DataDir() string      { return filepath.Join(c.BaseDir, "data") }
func (c *Config) MediaDir() string     { return filepath.Join(c.DataDir(), "media") }
func (c *Config) DownloadDir() string  { return filepath.Join(c.DataDir(), "downloads") }
func (c *Config) ProfileDir() string   { return filepath.Join(c.ProfileRoot, "generate") }
func (c *Config) AccountsFile() string { return filepath.Join(c.DataDir(), "accounts.json") }
func (c *Config) TasksFile() string    { return filepath.Join(c.DataDir(), "tasks.json") }
func (c *Config) EnvFile() string {
	if c.EnvPath != "" {
		return c.EnvPath
	}
	return filepath.Join(c.BaseDir, ".env")
}

func (c *Config) EnsureDirs() error {
	for _, dir := range []string{c.BaseDir, c.DataDir(), c.MediaDir(), c.DownloadDir(), c.ProfileDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// BaseURL returns the public API prefix (for example https://host/v1) or an
// empty string when no public base is configured.
func (c *Config) BaseURL() string {
	if c.PublicURL == "" {
		return ""
	}
	return c.PublicURL + "/v1"
}

// MediaURL returns an absolute media URL when a public base is configured and
// a relative one otherwise.
func (c *Config) MediaURL(name string) string {
	if c.PublicURL != "" {
		return c.PublicURL + "/v1/media/" + name
	}
	return "/v1/media/" + name
}

func (c *Config) FullURL(path string) string {
	if c.PublicURL != "" {
		return c.PublicURL + path
	}
	return path
}

// PersistEnv updates or appends one key in .env atomically.
func (c *Config) PersistEnv(key, value string) error {
	c.envMu.Lock()
	defer c.envMu.Unlock()
	path := c.EnvFile()
	data, err := os.ReadFile(path)
	var lines []string
	if err == nil {
		lines = strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	} else if !os.IsNotExist(err) {
		return err
	}
	found := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), key+"=") {
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	content := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".env-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ReadEnvFileValue reads one key from .env (used for GITHUB_TOKEN).
func (c *Config) ReadEnvFileValue(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	data, err := os.ReadFile(c.EnvFile())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, key+"=") {
			return strings.TrimSpace(strings.TrimPrefix(s, key+"="))
		}
	}
	return ""
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") || !strings.Contains(s, "=") {
			continue
		}
		parts := strings.SplitN(s, "=", 2)
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if current, exists := os.LookupEnv(key); !exists || (key == "MUSE2API_KEY" && strings.TrimSpace(current) == "") {
			_ = os.Setenv(key, value)
		}
	}
}

func DetectChromium(configured string) string {
	if configured != "" {
		if fileExists(configured) {
			return configured
		}
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
	}
	candidates := []string{
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/snap/bin/chromium",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"chromium",
		"chromium-browser",
		"microsoft-edge", "brave-browser",
		"google-chrome",
		"chrome",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, base := range []string{os.Getenv("LOCALAPPDATA"), os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), `C:\Program Files`, `C:\Program Files (x86)`} {
		if base != "" {
			for _, rel := range []string{`Google\Chrome\Application\chrome.exe`, `Microsoft\Edge\Application\msedge.exe`, `BraveSoftware\Brave-Browser\Applicationrave.exe`, `Chromium\Application\chrome.exe`} {
				candidates = append(candidates, filepath.Join(base, rel))
			}
		}
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
		if !filepath.IsAbs(c) {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		}
	}
	if configured != "" {
		return configured
	}
	if runtime.GOOS == "windows" {
		return "chrome.exe"
	}
	return "chromium"
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (c *Config) String() string {
	return fmt.Sprintf("muse2api %s (%s:%d, cdp:%d, chromium:%s)", c.Version, c.Host, c.Port, c.CDPPort, c.Chromium)
}
