package luban

import (
	"github.com/lush/blowball/internal/tool/skill"
)

// skillEntry is the JSON shape returned by luban_list_skills.
type skillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

// listSkills returns all skills visible to userID, with user skills overriding
// global skills of the same name.
func listSkills(loader *skill.Loader, userID string) ([]skillEntry, error) {
	skills := loader.List(userID)
	out := make([]skillEntry, 0, len(skills))
	for _, s := range skills {
		out = append(out, skillEntry{
			Name:        s.Name,
			Description: s.Description,
			Location:    s.Location,
		})
	}
	return out, nil
}
