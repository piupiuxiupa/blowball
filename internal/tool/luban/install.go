package luban

import (
	"context"
	"fmt"
	"io"
	"io/fs"
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

// installResult is the discriminated JSON shape returned by luban_install_skill.
// Kind distinguishes a successful install ("install") from an install-document
// return ("install-doc"), where the fetched body was not a skill and is handed
// back to the agent to read.
type installResult struct {
	// Kind discriminates the result: "install" (a skill was written) or
	// "install-doc" (the URL was an install document, not a skill; nothing
	// was written and Content holds the fetched text for the agent to read).
	Kind string `json:"kind"`
	// Installed reports whether a skill was written to the user skills dir.
	Installed bool `json:"installed"`

	// Install result fields (Kind == "install").
	Name      string `json:"name,omitempty"`
	Path      string `json:"path,omitempty"`
	Overwrite bool   `json:"overwrite,omitempty"`
	Note      string `json:"note,omitempty"`

	// Install-document fields (Kind == "install-doc").
	URL     string `json:"url,omitempty"`
	Content string `json:"content,omitempty"`
	Hint    string `json:"hint,omitempty"`
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
// single skill file. For git-repo URLs, skill selects a single sub-skill from
// the cloned collection and installs only that one. skill is ignored for
// single-file sources (a note is added to the result).
func (ins *installer) installSkill(ctx context.Context, urlStr, name, skillArg, userID string) (installResult, error) {
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
	overwrite := dirExists(filepath.Join(userSkillsDir, name))

	if isSingleSkillURL(u.Path) {
		return ins.installSingleFile(ctx, urlStr, userSkillsDir, name, overwrite, explicit, skillArg)
	}

	if skillArg != "" {
		return ins.installSubSkill(ctx, urlStr, skillArg, userSkillsDir, name, explicit)
	}

	return ins.installGitRepo(ctx, urlStr, userSkillsDir, name, overwrite, explicit)
}

// isSingleSkillURL reports whether the URL points to a single markdown file
// that should be downloaded rather than cloned.
func isSingleSkillURL(path string) bool {
	lower := strings.ToLower(path)
	return strings.HasSuffix(lower, ".md") || strings.HasSuffix(lower, "skill.md")
}

// installSingleFile downloads a SKILL.md from url and writes it to
// {userSkillsDir}/{name}/SKILL.md. The frontmatter name is preferred when no
// explicit name was requested. If the fetched body is not a valid SKILL.md (no
// frontmatter, or frontmatter missing name/description) it is returned as an
// install document rather than erroring, so the agent can read it and install
// the real source. If skillArg is set, the install proceeds normally and the
// result notes that skill does not apply to single-file sources.
func (ins *installer) installSingleFile(ctx context.Context, urlStr, userSkillsDir, name string, overwrite, explicit bool, skillArg string) (installResult, error) {
	body, err := ins.download(ctx, urlStr)
	if err != nil {
		return installResult{}, err
	}

	meta, _, perr := skill.ParseFrontmatter(body)
	if perr != nil || meta.Name == "" || meta.Description == "" {
		// Not a skill: hand the fetched content back as an install document so
		// the agent can read it, find the real source, and re-install.
		return installResult{
			Kind:    "install-doc",
			URL:     urlStr,
			Content: string(body),
			Hint:    "This URL is not a valid SKILL.md (missing or invalid frontmatter). Read the content above, determine the real skill source URL, and call luban_install_skill again with that URL. Use the optional `skill` parameter to select one sub-skill from a collection repo.",
		}, nil
	}

	// Prefer the frontmatter name unless the caller explicitly requested one.
	installedName := name
	if !explicit && meta.Name != name {
		if err := validateSkillName(meta.Name); err != nil {
			return installResult{}, fmt.Errorf("luban_install_skill: frontmatter name %q is invalid: %w", meta.Name, err)
		}
		installedName = meta.Name
	}

	targetDir := filepath.Join(userSkillsDir, installedName)
	overwrite = dirExists(targetDir) // recompute against the final name
	path := filepath.Join(targetDir, "SKILL.md")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: mkdir %q: %w", targetDir, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: write %q: %w", path, err)
	}

	res := installResult{
		Kind:      "install",
		Installed: true,
		Name:      installedName,
		Path:      targetDir,
		Overwrite: overwrite,
	}
	if skillArg != "" {
		res.Note = "the `skill` parameter does not apply to single-file sources; the SKILL.md was installed directly"
	}
	return res, nil
}

// installSubSkill clones url into a staging directory, discovers the
// sub-skills, selects the one matching skillArg (by frontmatter name, else by
// repo-relative subpath), copies only that sub-skill into
// {userSkillsDir}/{installed-name}, and discards the rest of the clone. The
// installed name is the explicit name override when given, otherwise the
// selected sub-skill's frontmatter name.
func (ins *installer) installSubSkill(ctx context.Context, urlStr, skillArg, userSkillsDir, name string, explicit bool) (installResult, error) {
	staging, err := os.MkdirTemp("", "luban-skill-*")
	if err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := ins.gitRunner(ctx, urlStr, staging); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: %w", err)
	}

	discovered := ins.loader.Discover(staging)

	// 1. Match by frontmatter name; require exactly one match, else fall through.
	var selected *skill.Skill
	var nameMatches []skill.Skill
	for i := range discovered {
		if discovered[i].Name == skillArg {
			nameMatches = append(nameMatches, discovered[i])
		}
	}
	if len(nameMatches) == 1 {
		m := nameMatches[0]
		selected = &m
	}

	// 2. Fall back to a repo-relative subpath whose directory holds a SKILL.md.
	if selected == nil {
		if candidate, ok := safeSubPath(staging, skillArg); ok {
			candidateSkill := filepath.Join(candidate, "SKILL.md")
			if data, rerr := os.ReadFile(candidateSkill); rerr == nil {
				if meta, _, perr := skill.ParseFrontmatter(data); perr == nil && meta.Name != "" && meta.Description != "" {
					meta.Path = candidateSkill
					selected = &meta
				}
			}
		}
	}

	if selected == nil {
		return installResult{}, fmt.Errorf("luban_install_skill: sub-skill %q not found in repo; available sub-skills: %s", skillArg, listDiscoveredNames(discovered))
	}

	// Determine the installed directory name.
	installedName := name
	if !explicit {
		switch {
		case selected.Name != "":
			installedName = selected.Name
		default:
			// Sub-skill matched by subpath with no frontmatter name: fall back
			// to the base of the requested subpath, not the full path.
			installedName = filepath.Base(skillArg)
		}
		if err := validateSkillName(installedName); err != nil {
			return installResult{}, fmt.Errorf("luban_install_skill: %w", err)
		}
	}

	if err := os.MkdirAll(userSkillsDir, 0o755); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: mkdir %q: %w", userSkillsDir, err)
	}
	targetDir := filepath.Join(userSkillsDir, installedName)
	overwrite := dirExists(targetDir)
	if overwrite {
		if err := os.RemoveAll(targetDir); err != nil {
			return installResult{}, fmt.Errorf("luban_install_skill: remove existing %q: %w", targetDir, err)
		}
	}

	srcDir := filepath.Dir(selected.Path)
	if err := copyDir(srcDir, targetDir); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: install sub-skill %q: %w", skillArg, err)
	}

	return installResult{
		Kind:      "install",
		Installed: true,
		Name:      installedName,
		Path:      targetDir,
		Overwrite: overwrite,
	}, nil
}

// installGitRepo clones url into {userSkillsDir}/{name}. If the cloned root
// contains a SKILL.md whose frontmatter name differs from name, the directory
// is renamed to match the frontmatter name.
func (ins *installer) installGitRepo(ctx context.Context, urlStr, userSkillsDir, name string, overwrite, explicit bool) (installResult, error) {
	if err := os.MkdirAll(userSkillsDir, 0o755); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: mkdir %q: %w", userSkillsDir, err)
	}

	targetDir := filepath.Join(userSkillsDir, name)
	if overwrite {
		if err := os.RemoveAll(targetDir); err != nil {
			return installResult{}, fmt.Errorf("luban_install_skill: remove existing %q: %w", targetDir, err)
		}
	}

	if err := ins.gitRunner(ctx, urlStr, targetDir); err != nil {
		return installResult{}, fmt.Errorf("luban_install_skill: %w", err)
	}

	// Prefer the frontmatter name from the root SKILL.md when present and no
	// explicit name was requested.
	if !explicit {
		rootSkill := filepath.Join(targetDir, "SKILL.md")
		if data, err := os.ReadFile(rootSkill); err == nil {
			meta, _, err := skill.ParseFrontmatter(data)
			if err == nil && meta.Name != "" && meta.Name != name {
				if err := validateSkillName(meta.Name); err != nil {
					return installResult{}, fmt.Errorf("luban_install_skill: frontmatter name %q is invalid: %w", meta.Name, err)
				}
				newDir := filepath.Join(userSkillsDir, meta.Name)
				if newDir != targetDir {
					if _, err := os.Stat(newDir); err == nil {
						if err := os.RemoveAll(newDir); err != nil {
							return installResult{}, fmt.Errorf("luban_install_skill: remove existing renamed dir %q: %w", newDir, err)
						}
					}
					if err := os.Rename(targetDir, newDir); err != nil {
						return installResult{}, fmt.Errorf("luban_install_skill: rename to frontmatter name: %w", err)
					}
					targetDir = newDir
					name = meta.Name
				}
			}
		}
	}

	return installResult{
		Kind:      "install",
		Installed: true,
		Name:      name,
		Path:      targetDir,
		Overwrite: overwrite,
	}, nil
}

// download fetches url with an optional size limit. Only HTTP 200 responses
// with a text content type are accepted; anything else is an error so a binary
// blob or error page is never mistaken for an install document.
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
	ct := resp.Header.Get("Content-Type")
	if !isTextContentType(ct) {
		return nil, fmt.Errorf("luban_install_skill: download returned non-text content type %q", ct)
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

// isTextContentType reports whether ct denotes a text body. An empty/missing
// content type is treated as text (common for .md fetches), as is any text/*
// type. Non-text types (images, archives, etc.) are rejected.
func isTextContentType(ct string) bool {
	ct = strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	if ct == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(ct), "text/")
}

// dirExists reports whether path exists on disk.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// safeSubPath joins base with sub only if sub stays within base: it rejects
// empty, absolute, or ".."-escaping values. It returns the joined path and ok.
func safeSubPath(base, sub string) (string, bool) {
	if sub == "" || filepath.IsAbs(sub) || strings.Contains(sub, "..") {
		return "", false
	}
	joined := filepath.Join(base, sub)
	rel, err := filepath.Rel(base, joined)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

// listDiscoveredNames returns the sorted, de-duplicated frontmatter names of
// discovered skills, formatted for an error message.
func listDiscoveredNames(skills []skill.Skill) string {
	if len(skills) == 0 {
		return "(none discovered)"
	}
	seen := make(map[string]struct{}, len(skills))
	var names []string
	for _, s := range skills {
		if _, ok := seen[s.Name]; ok {
			continue
		}
		seen[s.Name] = struct{}{}
		names = append(names, s.Name)
	}
	return strings.Join(names, ", ")
}

// copyDir recursively copies the directory src into dst, preserving file
// permissions. It is used to move a selected sub-skill out of a staging clone
// into the user skills directory (a copy rather than a rename because the two
// may live on different filesystems).
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("source %q is not a directory", src)
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		mode := 0o644
		if fi, err := d.Info(); err == nil {
			mode = int(fi.Mode().Perm())
			if mode == 0 {
				mode = 0o644
			}
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, os.FileMode(mode))
	})
}
