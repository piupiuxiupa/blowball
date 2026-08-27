package luban

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/skill"
)

// This file implements the skill market as luban's THIRD name-resolution
// source (skill-market capability): user > global > market. The local
// skill.Loader stays untouched (its disk-only purity is a design invariant);
// after a local miss the shared fallback below consults the user's market
// allowlist and resolves the entry to {data-dir}/skills-market{path}. A nil
// client means the capability is off and every function here is a no-op, so
// the nil-dependency wiring is byte-for-byte pre-capability behavior.

// marketSkillDir resolves name to its on-disk market directory after LOCAL
// resolution missed. It is the ONE market fallback shared by readSkill /
// resolveSkillSubdir / listSkills so the precedence rules live in a single
// place. A nil client, an empty user, or an allowlist miss returns ("", false).
func marketSkillDir(ctx context.Context, market *skillmarket.Client, name, userID string) (string, bool) {
	if market == nil || userID == "" {
		return "", false
	}
	allow := market.Allowlist(ctx, userID)
	return market.ResolveDir(allow, name)
}

// errMarketDirMissing is the disk-sync-lag error: the allowlist lists the
// skill (the API is the single source of visibility truth) but the operator
// sync has not landed its directory yet. Reads/listings then fail loudly with
// "directory not found" rather than silently hiding the entry.
func statMarketDir(dir, name, toolName string) error {
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: market skill %q: directory not found (disk sync incomplete)", toolName, name)
		}
		return fmt.Errorf("%s: stat market skill %q: %w", toolName, name, err)
	}
	return nil
}

// readMarketSkillBody reads the SKILL.md body (frontmatter stripped) from a
// resolved market skill directory, mirroring skill.Loader.Read's semantics:
// the per-file size cap and frontmatter handling are identical; only the
// "directory not found" disk-lag error is market-specific.
func readMarketSkillBody(dir, name string, maxSize int64) (string, error) {
	const toolName = "luban_read_skill"
	skillMD := filepath.Join(dir, "SKILL.md")
	info, err := os.Stat(skillMD)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: market skill %q: SKILL.md not found (disk sync incomplete)", toolName, name)
		}
		return "", fmt.Errorf("%s: stat market skill %q: %w", toolName, name, err)
	}
	if info.Size() > maxSize {
		return "", fmt.Errorf("%s: skill %q exceeds size limit (%d > %d)", toolName, name, info.Size(), maxSize)
	}
	data, err := os.ReadFile(skillMD)
	if err != nil {
		return "", fmt.Errorf("%s: read market skill %q: %w", toolName, name, err)
	}
	_, body, err := skill.ParseFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("%s: parse market skill %q: %w", toolName, name, err)
	}
	return string(body), nil
}

// readMarketSkillPath reads the text file at relPath inside a resolved market
// skill directory, mirroring skill.Loader.ReadPath's semantics EXACTLY —
// sub-path confinement via the shared skill.ValidateSubPath (absolute paths,
// ".." escapes, and symlinks resolving outside the skill directory are all
// rejected, identical to local skills), the per-file size cap, binary
// rejection, and frontmatter stripping — so market skills behave the same as
// local ones once the name resolves.
func readMarketSkillPath(dir, name, relPath string, maxSize int64) (string, error) {
	const toolName = "luban_read_skill"
	absPath, verr := skill.ValidateSubPath(dir, relPath)
	if verr != nil {
		return "", fmt.Errorf("%s: %w", toolName, verr)
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s: file not found: %q", toolName, relPath)
		}
		return "", fmt.Errorf("%s: stat %q: %w", toolName, relPath, err)
	}
	if info.Size() > maxSize {
		return "", fmt.Errorf("%s: %q exceeds size limit (%d > %d)", toolName, relPath, info.Size(), maxSize)
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("%s: read %q: %w", toolName, relPath, err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s: %q is a binary file; only text files are readable", toolName, relPath)
	}
	_, body, err := skill.ParseFrontmatter(data)
	if err != nil {
		return "", fmt.Errorf("%s: parse %q: %w", toolName, relPath, err)
	}
	return string(body), nil
}
