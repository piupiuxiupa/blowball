package luban

import (
	"context"
	"fmt"

	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/skill"
)

// readSkill returns the markdown body of the named skill. When path is empty
// it reads the skill's SKILL.md (backwards compatible); otherwise it reads the
// text file at path relative to the skill's directory root. YAML frontmatter
// is stripped in both cases. Name resolution is user > global > market
// (skill-market): local skills win, and after a local miss the market
// allowlist resolves the name to {data-dir}/skills-market{path} — disk-sync
// lag then surfaces as an explicit "directory not found" error rather than a
// silent miss. A nil market client keeps resolution purely local
// (byte-for-byte the pre-capability behavior).
func readSkill(ctx context.Context, loader *skill.Loader, market *skillmarket.Client, name, path, userID string) (string, error) {
	if err := validateSkillName(name); err != nil {
		return "", fmt.Errorf("luban_read_skill: %w", err)
	}
	// Local first (user skills override global skills of the same name).
	if _, ok := loader.SkillDir(name, userID); ok {
		var (
			body []byte
			err  error
		)
		if path == "" {
			body, err = loader.Read(name, userID)
		} else {
			body, err = loader.ReadPath(name, path, userID)
		}
		if err != nil {
			return "", err
		}
		return string(body), nil
	}
	// Market fallback (third source).
	if dir, ok := marketSkillDir(ctx, market, name, userID); ok {
		if err := statMarketDir(dir, name, "luban_read_skill"); err != nil {
			return "", err
		}
		if path == "" {
			return readMarketSkillBody(dir, name, loader.MaxSize())
		}
		return readMarketSkillPath(dir, name, path, loader.MaxSize())
	}
	return "", fmt.Errorf("luban_read_skill: skill %q not found", name)
}
