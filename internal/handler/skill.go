package handler

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/skillmarket"
	"github.com/lush/blowball/internal/store/fs"
)

// SkillHandler owns GET /api/v1/skills. It scans the authenticated user's
// skills directory and returns each file as a usable skill entry. When the
// skill market is wired (skill-market capability), the response additionally
// merges the user's market allowlist entries: name/description from the
// market API, location "skill_market", local-first on name conflicts, and a
// market failure degrades to the local-only list with HTTP 200 (fail-closed).
type SkillHandler struct {
	fsSvc  *fs.Store
	market *skillmarket.Client
}

// NewSkillHandler wires the handler with the fs store and the optional
// skill-market client (nil = capability off = the pre-capability response).
func NewSkillHandler(fsSvc *fs.Store, market *skillmarket.Client) *SkillHandler {
	return &SkillHandler{fsSvc: fsSvc, market: market}
}

// skillEntry is one element of the GET /api/v1/skills response array. Name is
// the skill directory name (the canonical skill identifier). The two
// omitempty fields are the skill-market additions (skill-market capability):
// market entries carry location "skill_market" plus the API description;
// local entries serialize exactly as before the capability (no location key).
type skillEntry struct {
	Name        string `json:"name"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	UpdateTime  string `json:"update_time"`
	Location    string `json:"location,omitempty"`
	Description string `json:"description,omitempty"`
}

// List handles GET /api/v1/skills. Returns 200 with a (possibly empty) array
// of skill entries sorted by name. A missing skills directory returns an empty
// array. Each local skill is discovered as {skill-name}/SKILL.md under the
// user's skills directory; market entries come from the user's allowlist (the
// API is the visibility truth, so they appear regardless of disk sync state)
// and never stat the disk here.
func (h *SkillHandler) List(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)

	skillsDir := h.fsSvc.UserSkills(userID)
	entries, err := os.ReadDir(skillsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "list skills failed"))
		return
	}

	out := make([]skillEntry, 0, len(entries))
	local := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(skillsDir, e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		local[e.Name()] = struct{}{}
		out = append(out, skillEntry{
			Name:       e.Name(),
			Filename:   e.Name(),
			Size:       info.Size(),
			UpdateTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}

	// Skill-market merge (skill-market capability). This endpoint authenticates
	// with the SAME login JWT the request carries, so the allowlist fetch uses
	// the request's own Authorization header — no turn-context pipeline needed.
	// Fail-closed: any fetch failure yields an empty allowlist inside Client
	// and the response degrades to the local list, still HTTP 200.
	if h.market != nil {
		rawToken, _ := middleware.BearerToken(c.GetHeader("Authorization"))
		ctx := skillmarket.WithToken(context.Background(), rawToken)
		for _, e := range h.market.Allowlist(ctx, userID) {
			if e.Name == "" {
				continue
			}
			if _, isLocal := local[e.Name]; isLocal {
				continue // local wins on name conflicts
			}
			out = append(out, skillEntry{
				Name:        e.Name,
				Filename:    e.Name,
				Location:    skillmarket.LocationSkillMarket,
				Description: e.Description,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	c.JSON(http.StatusOK, gin.H{"skills": out})
}
