package luban

import (
	"context"
	"sort"

	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/tool/skill"
)

// skillEntry is the JSON shape returned by luban_list_skills.
type skillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
}

// listSkills returns all skills visible to userID: local skills (user skills
// overriding global skills of the same name) merged with the user's skill
// market allowlist when the capability is wired (skill-market). Market entries
// carry location "skill_market" with the name/description from the market API
// — the API is the visibility truth, so entries appear even when their disk
// sync lags — and any name conflict resolves local-first (user > global >
// market). A nil market client (capability off) yields the pure local list,
// byte-for-byte the pre-capability behavior; a market failure degrades
// fail-closed to the local list inside Allowlist.
func listSkills(ctx context.Context, loader *skill.Loader, market *skillmarket.Client, userID string) ([]skillEntry, error) {
	skills := loader.List(userID)
	out := make([]skillEntry, 0, len(skills))
	local := make(map[string]struct{}, len(skills))
	for _, s := range skills {
		local[s.Name] = struct{}{}
		out = append(out, skillEntry{
			Name:        s.Name,
			Description: s.Description,
			Location:    s.Location,
		})
	}
	if market != nil {
		for _, e := range market.Allowlist(ctx, userID) {
			if e.Name == "" {
				continue
			}
			if _, isLocal := local[e.Name]; isLocal {
				continue // local wins: user > global > market
			}
			out = append(out, skillEntry{
				Name:        e.Name,
				Description: e.Description,
				Location:    skillmarket.LocationSkillMarket,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
