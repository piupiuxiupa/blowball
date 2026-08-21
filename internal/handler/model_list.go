package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/lush/blowball/internal/config"
)

// ModelListHandler owns GET /api/v1/models (per-request-model-selection). It
// is pure config echo — no store, no agent dependency — so it lives in the
// api route partition next to the skills list.
type ModelListHandler struct {
	catalog []config.ModelCatalogEntry
	def     string
	defEff  string
}

// NewModelListHandler wires the handler with the effective model catalog, the
// default model name, and the deployment default reasoning effort
// (openai.default_reasoning_effort; pass "none" when unset — config load
// normalizes it). The frontend uses default_reasoning_effort to preselect the
// effort picker.
func NewModelListHandler(catalog []config.ModelCatalogEntry, def, defaultEffort string) *ModelListHandler {
	return &ModelListHandler{catalog: catalog, def: def, defEff: defaultEffort}
}

// modelEntry is one element of the GET /api/v1/models response array.
type modelEntry struct {
	Name                string `json:"name"`
	MaxContextTokens    int    `json:"max_context_tokens"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Thinking            bool   `json:"thinking"`
}

// List handles GET /api/v1/models. Returns 200 with the selectable model
// catalog, the default model name, and the deployment default reasoning
// effort; a thinking flag tells the frontend whether reasoning_effort values
// other than none are valid for the model. Each entry also carries its
// max_completion_tokens output quota (per-model-completion-budget); the
// per-entry length_continue sub-block is deliberately NOT exposed (an
// operator-side continuation switch, not a user selection axis).
func (h *ModelListHandler) List(c *gin.Context) {
	models := make([]modelEntry, 0, len(h.catalog))
	for _, e := range h.catalog {
		models = append(models, modelEntry{
			Name:                e.Name,
			MaxContextTokens:    e.MaxContextTokens,
			MaxCompletionTokens: e.MaxCompletionTokens,
			Thinking:            e.Thinking,
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"models":                   models,
		"default":                  h.def,
		"default_reasoning_effort": h.defEff,
	})
}
