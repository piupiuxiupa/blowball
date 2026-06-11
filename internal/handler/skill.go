package handler

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/middleware"
	"github.com/lush/blowball/internal/store/fs"
)

// SkillHandler owns GET /api/v1/skills. It scans the authenticated user's
// skills directory and returns each file as a usable skill entry.
type SkillHandler struct {
	fsSvc *fs.Store
}

// NewSkillHandler wires the handler with the fs store.
func NewSkillHandler(fsSvc *fs.Store) *SkillHandler {
	return &SkillHandler{fsSvc: fsSvc}
}

// skillEntry is one element of the GET /api/v1/skills response array. Name is
// the filename without its extension (the canonical skill identifier);
// Filename is the full on-disk name.
type skillEntry struct {
	Name       string `json:"name"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	UpdateTime string `json:"update_time"`
}

// List handles GET /api/v1/skills. Returns 200 with a (possibly empty) array
// of skill entries sorted by name. A missing skills directory returns an empty
// array (the user's skills/ subdir is created on first login, so this is the
// expected steady state for new users).
func (h *SkillHandler) List(c *gin.Context) {
	userID := middleware.UserIDFromCtx(c)

	skillsDir := h.fsSvc.UserSkills(userID)
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.JSON(http.StatusOK, gin.H{"skills": []skillEntry{}})
			return
		}
		c.JSON(http.StatusInternalServerError, errorBody("INTERNAL", "list skills failed"))
		return
	}

	out := make([]skillEntry, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		out = append(out, skillEntry{
			Name:       skillNameFromFilename(name),
			Filename:   name,
			Size:       info.Size(),
			UpdateTime: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	c.JSON(http.StatusOK, gin.H{"skills": out})
}

// skillNameFromFilename strips the file extension to produce the canonical
// skill identifier. Files with no extension are returned verbatim.
func skillNameFromFilename(filename string) string {
	ext := filepath.Ext(filename)
	if ext == "" {
		return filename
	}
	return strings.TrimSuffix(filename, ext)
}
