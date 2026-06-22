package luban

import (
	"fmt"

	"github.com/lush/blowball/internal/tool/skill"
)

// readSkill returns the markdown body of the named skill, with YAML frontmatter
// stripped. User skills take precedence over global skills.
func readSkill(loader *skill.Loader, name, userID string) (string, error) {
	if err := validateSkillName(name); err != nil {
		return "", fmt.Errorf("luban_read_skill: %w", err)
	}
	body, err := loader.Read(name, userID)
	if err != nil {
		return "", err
	}
	return string(body), nil
}
