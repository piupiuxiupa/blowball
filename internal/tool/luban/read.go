package luban

import (
	"fmt"

	"github.com/lush/blowball/internal/tool/skill"
)

// readSkill returns the markdown body of the named skill. When path is empty
// it reads the skill's SKILL.md (backwards compatible); otherwise it reads the
// .md file at path relative to the skill's directory root. YAML frontmatter is
// stripped in both cases. User skills take precedence over global skills.
func readSkill(loader *skill.Loader, name, path, userID string) (string, error) {
	if err := validateSkillName(name); err != nil {
		return "", fmt.Errorf("luban_read_skill: %w", err)
	}
	var (
		body []byte
		err   error
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
