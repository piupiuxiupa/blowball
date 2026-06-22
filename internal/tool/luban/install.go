package luban

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lush/blowball/internal/tool/skill"
)

// MaxInstallSize is the default maximum download size for a single SKILL.md
// installed via luban_install_skill.
const MaxInstallSize int64 = 500 * 1024 // 500KB

// installResult is the JSON shape returned by luban_install_skill.
type installResult struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Overwrite bool   `json:"overwrite"`
}

// installer holds the dependencies for luban_install_skill.
type installer struct {
	loader     *skill.Loader
	userDirFn  func(userID string) string
	httpClient *http.Client
	maxSize    int64
	gitRunner  func(ctx context.Context, urlStr, targetDir string) error
}

func newInstaller(loader *skill.Loader, userDirFn func(userID string) string) *installer {
	return &installer{
		loader:     loader,
		userDirFn:  userDirFn,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		maxSize:    MaxInstallSize,
		gitRunner:  defaultGitRunner,
	}
}

func defaultGitRunner(ctx context.Context, urlStr, targetDir string) error {
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", urlStr, targetDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w\n%s", err, string(out))
	}
	return nil
}

// installSkill installs a skill or skill collection from url into the user's
// skills directory. If name is empty it is inferred from the URL path. Git repo
// URLs are cloned; URLs ending in ".md" or "SKILL.md" are downloaded as a
// single skill file.
func (ins *installer) installSkill(ctx context.Context, urlStr, name, userID string) (installResult, error) {
	var res installResult
	if userID == "" {
		return res, fmt.Errorf("luban_install_skill: no userID in context")
	}

	u, err := url.Parse(urlStr)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return res, fmt.Errorf("luban_install_skill: invalid URL %q", urlStr)
	}

	explicit := true
	if name == "" {
		explicit = false
		name = normalizeName(filepath.Base(u.Path))
		if name == "" || name == "." {
			return res, fmt.Errorf("luban_install_skill: cannot infer skill name from URL")
		}
	}
	if err := validateSkillName(name); err != nil {
		return res, fmt.Errorf("luban_install_skill: %w", err)
	}

	userSkillsDir := ins.userDirFn(userID)
	targetDir := filepath.Join(userSkillsDir, name)

	overwrite := false
	if _, err := os.Stat(targetDir); err == nil {
		overwrite = true
	}

	if isSingleSkillURL(u.Path) {
		installedName, path, err := ins.installSingleFile(ctx, urlStr, userSkillsDir, name, overwrite, explicit)
		if err != nil {
			return res, err
		}
		return installResult{Name: installedName, Path: path, Overwrite: overwrite}, nil
	}

	installedName, path, err := ins.installGitRepo(ctx, urlStr, userSkillsDir, name, overwrite, explicit)
	if err != nil {
		return res, err
	}
	return installResult{Name: installedName, Path: path, Overwrite: overwrite}, nil
}

// isSingleSkillURL reports whether the URL points to a single markdown file
// that should be downloaded rather than cloned.
func isSingleSkillURL(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, "skill.md")
}

// installSingleFile downloads a SKILL.md from url and writes it to
// {userSkillsDir}/{name}/SKILL.md. The frontmatter name is preferred when no
// explicit name was requested.
func (ins *installer) installSingleFile(ctx context.Context, urlStr, userSkillsDir, name string, overwrite, explicit bool) (string, string, error) {
	_ = overwrite // single-file install always overwrites the target SKILL.md
	body, err := ins.download(ctx, urlStr)
	if err != nil {
		return "", "", err
	}

	meta, _, err := skill.ParseFrontmatter(body)
	if err != nil || meta.Name == "" || meta.Description == "" {
		return "", "", fmt.Errorf("luban_install_skill: downloaded file is not a valid SKILL.md")
	}

	// Prefer the frontmatter name unless the caller explicitly requested one.
	if !explicit && meta.Name != name {
		if err := validateSkillName(meta.Name); err != nil {
			return "", "", fmt.Errorf("luban_install_skill: frontmatter name %q is invalid: %w", meta.Name, err)
		}
		name = meta.Name
	}

	targetDir := filepath.Join(userSkillsDir, name)
	path := filepath.Join(targetDir, "SKILL.md")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return "", "", fmt.Errorf("luban_install_skill: mkdir %q: %w", targetDir, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return "", "", fmt.Errorf("luban_install_skill: write %q: %w", path, err)
	}
	return name, targetDir, nil
}

// installGitRepo clones url into {userSkillsDir}/{name}. If the cloned root
// contains a SKILL.md whose frontmatter name differs from name, the directory
// is renamed to match the frontmatter name.
func (ins *installer) installGitRepo(ctx context.Context, urlStr, userSkillsDir, name string, overwrite, explicit bool) (string, string, error) {
	if err := os.MkdirAll(userSkillsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("luban_install_skill: mkdir %q: %w", userSkillsDir, err)
	}

	targetDir := filepath.Join(userSkillsDir, name)
	if overwrite {
		if err := os.RemoveAll(targetDir); err != nil {
			return "", "", fmt.Errorf("luban_install_skill: remove existing %q: %w", targetDir, err)
		}
	}

	if err := ins.gitRunner(ctx, urlStr, targetDir); err != nil {
		return "", "", fmt.Errorf("luban_install_skill: %w", err)
	}

	// Prefer the frontmatter name from the root SKILL.md when present and no
	// explicit name was requested.
	if !explicit {
		rootSkill := filepath.Join(targetDir, "SKILL.md")
		if data, err := os.ReadFile(rootSkill); err == nil {
			meta, _, err := skill.ParseFrontmatter(data)
			if err == nil && meta.Name != "" && meta.Name != name {
				if err := validateSkillName(meta.Name); err != nil {
					return "", "", fmt.Errorf("luban_install_skill: frontmatter name %q is invalid: %w", meta.Name, err)
				}
				newDir := filepath.Join(userSkillsDir, meta.Name)
				if newDir != targetDir {
					if _, err := os.Stat(newDir); err == nil {
						if err := os.RemoveAll(newDir); err != nil {
							return "", "", fmt.Errorf("luban_install_skill: remove existing renamed dir %q: %w", newDir, err)
						}
					}
					if err := os.Rename(targetDir, newDir); err != nil {
						return "", "", fmt.Errorf("luban_install_skill: rename to frontmatter name: %w", err)
					}
					targetDir = newDir
					name = meta.Name
				}
			}
		}
	}

	return name, targetDir, nil
}

// download fetches url with an optional size limit.
func (ins *installer) download(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("luban_install_skill: create request: %w", err)
	}
	resp, err := ins.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("luban_install_skill: download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("luban_install_skill: download returned status %d", resp.StatusCode)
	}

	lr := io.LimitReader(resp.Body, ins.maxSize+1)
	body, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("luban_install_skill: read body: %w", err)
	}
	if int64(len(body)) > ins.maxSize {
		return nil, fmt.Errorf("luban_install_skill: downloaded file exceeds size limit (%d bytes)", ins.maxSize)
	}
	return body, nil
}
