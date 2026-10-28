package api

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/muse2api/muse2api/internal/web"
)

type UpdateManager struct {
	s *Server

	opMu sync.Mutex
	mu   sync.Mutex
	ts   time.Time
	data map[string]any
}

func NewUpdateManager(s *Server) *UpdateManager {
	return &UpdateManager{s: s}
}

type repoInfo struct {
	Owner   string
	Name    string
	Web     string
	Raw     string
	API     string
	Tarball string
	Clone   string
}

func parseRepo(raw, branch string) *repoInfo {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	raw = strings.TrimPrefix(raw, "https://github.com/")
	raw = strings.TrimPrefix(raw, "http://github.com/")
	parts := strings.Split(raw, "/")
	if len(parts) < 2 {
		return nil
	}
	owner, name := parts[0], parts[1]
	if branch == "" {
		branch = "main"
	}
	return &repoInfo{
		Owner:   owner,
		Name:    name,
		Web:     "https://github.com/" + owner + "/" + name,
		Raw:     fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/version.json", owner, name, branch),
		API:     fmt.Sprintf("https://api.github.com/repos/%s/%s/commits?sha=%s&per_page=5", owner, name, branch),
		Tarball: fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/refs/heads/%s", owner, name, branch),
		Clone:   fmt.Sprintf("https://github.com/%s/%s.git", owner, name),
	}
}

func (m *UpdateManager) Check(force bool) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if !force && m.data != nil && now.Sub(m.ts) < 90*time.Second {
		return m.data
	}

	cfg := m.s.cfg
	repo := parseRepo(cfg.RepoURL, cfg.RepoBranch)
	localVerObj := web.VersionData()
	localVersion := fmt.Sprint(localVerObj["version"])
	if localVersion == "" || localVersion == "<nil>" {
		localVersion = "2.0.0"
	}
	hasGit := dirExists(filepath.Join(cfg.BaseDir, ".git"))
	localSHA, localMsg := "", ""
	var localTS int64
	if hasGit {
		if out, err := runGit(cfg.BaseDir, nil, "rev-parse", "--short", "HEAD"); err == nil {
			localSHA = strings.TrimSpace(out)
		}
		if out, err := runGit(cfg.BaseDir, nil, "log", "-1", "--format=%s||%ct"); err == nil {
			if parts := strings.SplitN(strings.TrimSpace(out), "||", 2); len(parts) == 2 {
				localMsg = parts[0]
				localTS, _ = strconv.ParseInt(parts[1], 10, 64)
			}
		}
	}
	if localSHA == "" {
		if data, err := os.ReadFile(filepath.Join(cfg.DataDir(), ".installed_sha")); err == nil {
			localSHA = strings.TrimSpace(string(data))
			if len(localSHA) > 7 {
				localSHA = localSHA[:7]
			}
		}
	}

	remoteVersion := localVersion
	highlights := toStringSlice(localVerObj["highlights"])
	highlightsEN := toStringSlice(localVerObj["highlights_en"])
	if repo != nil {
		client := &http.Client{Timeout: 6 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s?t=%d", repo.Raw, now.Unix()), nil)
		if resp, err := client.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var rv map[string]any
				if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rv) == nil {
					if v := fmt.Sprint(rv["version"]); v != "" && v != "<nil>" {
						remoteVersion = v
					}
					if list := toStringSlice(rv["highlights"]); len(list) > 0 {
						highlights = list
						if en := toStringSlice(rv["highlights_en"]); len(en) > 0 {
							highlightsEN = en
						} else if remoteVersion == localVersion {
							// Keep the local English copy only when the versions match.
						} else {
							highlightsEN = nil
						}
					}
				}
			}
		}
	}

	remoteSHA, remoteMsg, remoteTime := "", "", ""
	var recent []any
	if repo != nil {
		token := cfg.ReadEnvFileValue("GITHUB_TOKEN")
		client := &http.Client{Timeout: 6 * time.Second}
		req, _ := http.NewRequest(http.MethodGet, repo.API, nil)
		req.Header.Set("Accept", "application/vnd.github.v3+json")
		req.Header.Set("User-Agent", "muse2api-updater")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if resp, err := client.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				var commits []map[string]any
				if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&commits) == nil {
					for idx, c := range commits {
						sha := fmt.Sprint(c["sha"])
						if len(sha) > 7 {
							sha = sha[:7]
						}
						commit, _ := c["commit"].(map[string]any)
						msg := ""
						if commit != nil {
							if s, ok := commit["message"].(string); ok {
								msg = strings.Split(s, "\n")[0]
							}
						}
						date := ""
						if commit != nil {
							if committer, ok := commit["committer"].(map[string]any); ok {
								date, _ = committer["date"].(string)
							}
						}
						url, _ := c["html_url"].(string)
						if url == "" {
							url = repo.Web + "/commit/" + sha
						}
						if idx == 0 {
							remoteSHA, remoteMsg, remoteTime = sha, msg, date
						}
						recent = append(recent, map[string]any{"sha": sha, "message": msg, "date": date, "url": url})
					}
				}
			}
		}
	}

	if localSHA == "" && remoteSHA != "" && localVersion == remoteVersion {
		localSHA = remoteSHA
		_ = os.WriteFile(filepath.Join(cfg.DataDir(), ".installed_sha"), []byte(remoteSHA), 0o644)
	}

	hasUpdate := false
	if remoteSHA != "" && localSHA != "" {
		hasUpdate = remoteSHA != localSHA
	} else if remoteVersion != "" && localVersion != "" {
		hasUpdate = compareVersions(remoteVersion, localVersion) > 0
	}
	repoURL := ""
	if repo != nil {
		repoURL = repo.Web
	}
	data := map[string]any{
		"repo_url":       repoURL,
		"has_git":        hasGit,
		"local_version":  localVersion,
		"remote_version": remoteVersion,
		"local_sha":      localSHA,
		"local_msg":      localMsg,
		"local_ts":       localTS,
		"remote_sha":     remoteSHA,
		"remote_msg":     remoteMsg,
		"remote_time":    remoteTime,
		"has_update":     hasUpdate,
		"up_to_date":     !hasUpdate && (remoteSHA != "" || remoteVersion != ""),
		"highlights":     highlights,
		"highlights_en":  highlightsEN,
		"recent_commits": recent,
		"checked_at":     now.Unix(),
	}
	m.ts = now
	m.data = data
	return data
}

func (m *UpdateManager) Upgrade(ctx context.Context) (map[string]any, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	cfg := m.s.cfg
	repo := parseRepo(cfg.RepoURL, cfg.RepoBranch)
	if repo == nil {
		return nil, httpError(400, "update source is not configured; set MUSE2API_REPO to a GitHub repository")
	}
	token := cfg.ReadEnvFileValue("GITHUB_TOKEN")
	cloneURL := repo.Clone
	if token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, repo.Owner, repo.Name)
	}
	via := ""
	if _, err := exec.LookPath("git"); err == nil {
		if dirExists(filepath.Join(cfg.BaseDir, ".git")) {
			status, err := runGit(cfg.BaseDir, nil, "status", "--porcelain", "--untracked-files=no")
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(status) != "" {
				return nil, httpError(409, "save local source changes before updating")
			}
		}
		if err := ensureGitRepo(cfg.BaseDir, cloneURL, cfg.RepoBranch); err != nil {
			return nil, err
		}
		if err := installGit(cfg.BaseDir, cfg.RepoBranch); err != nil {
			return nil, err
		}
		via = "git"
	}

	if via == "" {
		client := &http.Client{Timeout: 60 * time.Second}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, repo.Tarball, nil)
		resp, err := client.Do(req)
		if err != nil {
			return nil, httpError(502, "failed to download the GitHub update archive: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, httpError(502, "failed to download the GitHub update archive (HTTP %d)", resp.StatusCode)
		}
		if err := extractTarGz(resp.Body, cfg.BaseDir); err != nil {
			return nil, httpError(500, "failed to extract the update archive: %v", err)
		}
		via = "tarball"
	}

	rebuildOutput := ""
	if cfg.RebuildCommand != "" {
		out, err := runShell(cfg.BaseDir, cfg.RebuildCommand)
		rebuildOutput = out
		if err != nil {
			return nil, httpError(500, "source update succeeded but the rebuild command failed: %v\n%s", err, out)
		}
	}

	m.mu.Lock()
	m.ts = time.Time{}
	m.data = nil
	m.mu.Unlock()
	status := m.Check(true)
	if sha, _ := status["remote_sha"].(string); sha != "" {
		_ = os.WriteFile(filepath.Join(cfg.DataDir(), ".installed_sha"), []byte(sha), 0o644)
		status["local_sha"] = sha
		status["has_update"] = false
		status["up_to_date"] = true
	}
	res := map[string]any{
		"ok":      true,
		"via":     via,
		"message": fmt.Sprintf("Updated to %v (%v)", status["remote_version"], status["remote_sha"]),
		"status":  status,
	}
	if rebuildOutput != "" {
		res["rebuild_output"] = rebuildOutput
	}
	if cfg.RebuildCommand == "" {
		res["note"] = "The source was updated. Rebuild and restart the binary to run the new code (set MUSE2API_REBUILD_CMD to automate this)."
	}
	return res, nil
}

func (m *UpdateManager) Push(message, newToken string) (map[string]any, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if dirExists(filepath.Join(m.s.cfg.BaseDir, ".git")) {
		if err := checkStaged(m.s.cfg.BaseDir); err != nil {
			return nil, err
		}
	}
	cfg := m.s.cfg
	repo := parseRepo(cfg.RepoURL, cfg.RepoBranch)
	if repo == nil {
		return nil, httpError(400, "update source is not configured; set MUSE2API_REPO to a GitHub repository")
	}
	if strings.TrimSpace(message) == "" {
		message = "chore: sync update (" + time.Now().Format("2006-01-02 15:04:05") + ")"
	}
	token := strings.TrimSpace(newToken)
	if token != "" {
		if err := cfg.PersistEnv("GITHUB_TOKEN", token); err != nil {
			return nil, err
		}
	}
	if token == "" {
		token = cfg.ReadEnvFileValue("GITHUB_TOKEN")
	}
	cloneURL := repo.Clone
	if token != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, repo.Owner, repo.Name)
	}
	if err := ensureGitRepo(cfg.BaseDir, cloneURL, cfg.RepoBranch); err != nil {
		return nil, httpError(500, "failed to prepare the git repository: %v", err)
	}
	if err := basePush(cfg.BaseDir, cfg.RepoBranch); err != nil {
		return nil, err
	}
	tracked := trackedRepoTargets()
	existing := []string{}
	for _, p := range tracked {
		if _, err := os.Stat(filepath.Join(cfg.BaseDir, p)); err == nil {
			existing = append(existing, p)
		} else if tracked, err := runGit(cfg.BaseDir, nil, "ls-files", "--", p); err != nil {
			return nil, err
		} else if tracked != "" {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return nil, httpError(400, "no tracked project files were found")
	}
	args := append([]string{"add", "-A", "--"}, existing...)
	if _, err := runGit(cfg.BaseDir, nil, args...); err != nil {
		return nil, httpError(500, "git add failed: %v", err)
	}
	statusOut, err := runGit(cfg.BaseDir, nil, append([]string{"status", "--porcelain", "--"}, existing...)...)
	if err != nil {
		return nil, err
	}
	committed := strings.TrimSpace(statusOut) != ""
	if committed {
		if _, err := runGit(cfg.BaseDir, nil, "commit", "-m", message); err != nil {
			return nil, httpError(500, "git commit failed: %v", err)
		}
	}
	if _, err := runGit(cfg.BaseDir, nil, "push", "origin", "HEAD:"+cfg.RepoBranch); err != nil {
		return nil, httpError(500, "git push failed: %v", err)
	}
	m.mu.Lock()
	m.ts = time.Time{}
	m.data = nil
	m.mu.Unlock()
	status := m.Check(true)
	return map[string]any{"ok": true, "committed": committed, "message": "Committed and pushed to the GitHub repository", "status": status}, nil
}

func ensureGitRepo(baseDir, cloneURL, branch string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return err
	}
	if !dirExists(filepath.Join(baseDir, ".git")) {
		if _, err := runGit(baseDir, nil, "init", "-b", branch); err != nil {
			if _, err2 := runGit(baseDir, nil, "init"); err2 != nil {
				return err2
			}
		}
		if _, err := runGit(baseDir, nil, "remote", "add", "origin", cloneURL); err != nil {
			return err
		}
	} else {
		if _, err := runGit(baseDir, nil, "remote", "set-url", "origin", cloneURL); err != nil {
			return err
		}
	}
	if _, err := runGit(baseDir, nil, "config", "user.name", "muse2api"); err != nil {
		return err
	}
	if _, err := runGit(baseDir, nil, "config", "user.email", "muse2api@users.noreply.github.com"); err != nil {
		return err
	}
	return nil
}

func runGit(baseDir string, extraEnv []string, args ...string) (string, error) {
	cmdArgs := append([]string{"-c", "safe.directory=" + baseDir}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Dir = baseDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func runShell(baseDir, command string) (string, error) {
	var cmd *exec.Cmd
	if os.PathSeparator == '\\' {
		cmd = exec.Command("cmd", "/C", command)
	} else {
		cmd = exec.Command("sh", "-c", command)
	}
	cmd.Dir = baseDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	protected := map[string]bool{".env": true, "data/accounts.json": true, "data/tasks.json": true}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.ToSlash(hdr.Name)
		parts := strings.SplitN(name, "/", 2)
		if len(parts) < 2 || parts[1] == "" {
			continue
		}
		rel := parts[1]
		if strings.Contains(rel, "..") || protected[rel] {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		cleanDest := filepath.Clean(dest) + string(os.PathSeparator)
		if !strings.HasPrefix(filepath.Clean(target)+string(os.PathSeparator), cleanDest) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(target, 0o755)
		case tar.TypeReg, tar.TypeRegA:
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, 128<<20)); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func toStringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		if list, ok := v.([]string); ok {
			return list
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

var digitsRE = regexp.MustCompile(`\d+`)

func compareVersions(a, b string) int {
	pa := versionParts(a)
	pb := versionParts(b)
	n := len(pa)
	if len(pb) > n {
		n = len(pb)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(pa) {
			av = pa[i]
		}
		if i < len(pb) {
			bv = pb[i]
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionParts(v string) []int {
	matches := digitsRE.FindAllString(v, -1)
	out := make([]int, 0, len(matches))
	for _, m := range matches {
		n, _ := strconv.Atoi(m)
		out = append(out, n)
	}
	return out
}

// trackedRepoTargets is shared with tests.
func trackedRepoTargets() []string {
	out := []string{"go.mod", "go.sum", "cmd", "internal", "tools", "README.md", "version.json",
		"Dockerfile", "docker-compose.yml", ".env.example", ".gitignore", "LICENSE", "deploy"}
	sort.Strings(out)
	return out
}

// installGit refuses an incomplete install rather than falling back over modified files.
func installGit(baseDir, branch string) error {
	if out, err := runGit(baseDir, nil, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch failed: %w: %s", err, out)
	}
	if out, err := runGit(baseDir, nil, "checkout", "--detach", "origin/"+branch); err != nil {
		return fmt.Errorf("source checkout failed: %w: %s", err, out)
	}
	return nil
}
func checkStaged(baseDir string) error {
	out, err := runGit(baseDir, nil, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	for _, path := range strings.Split(out, "\x00") {
		if path == "" {
			continue
		}
		allowed := false
		for _, target := range trackedRepoTargets() {
			if path == target || strings.HasPrefix(path, target+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			return httpError(409, "staged file outside project targets: %s; unstage it before pushing", path)
		}
	}
	return nil
}

// basePush gives source-only installations the remote ancestry without replacing working files.
func basePush(baseDir, branch string) error {
	if _, err := runGit(baseDir, nil, "rev-parse", "--verify", "HEAD"); err == nil {
		return nil
	}
	out, err := runGit(baseDir, nil, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	} // A genuinely empty remote accepts an initial commit.
	if out, err := runGit(baseDir, nil, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("fetch push base: %w: %s", err, out)
	}
	if out, err := runGit(baseDir, nil, "reset", "--mixed", "origin/"+branch); err != nil {
		return fmt.Errorf("set push base: %w: %s", err, out)
	}
	return nil
}
