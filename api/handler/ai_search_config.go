package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"opencsg.com/csghub-server/api/httpbase"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/component"
)

// AISearchConfigHandler serves the admin endpoints for the dataset AI search
// LLM configuration.
type AISearchConfigHandler struct {
	comp component.AISearchConfigComponent
}

func NewAISearchConfigHandler(comp component.AISearchConfigComponent) *AISearchConfigHandler {
	return &AISearchConfigHandler{comp: comp}
}

// GetAISearchLLMConfig godoc
// @Summary      Get the dataset AI search LLM configuration
// @Description  Returns the effective configuration (admin-saved values override env/toml)
// @Tags         Admin
// @Produce      json
// @Success      200  {object}  types.AISearchLLMConfig
// @Failure      500  {object}  httpbase.R
// @Router       /admin/ai_search/config [get]
func (h *AISearchConfigHandler) GetLLMConfig(ctx *gin.Context) {
	cfg, err := h.comp.GetLLMConfig(ctx.Request.Context())
	if err != nil {
		httpbase.ServerError(ctx, err)
		return
	}
	ctx.JSON(http.StatusOK, cfg)
}

// UpdateAISearchLLMConfig godoc
// @Summary      Update the dataset AI search LLM configuration
// @Description  Persists the configuration; takes effect immediately for new chat requests
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Param        req body types.AISearchLLMConfig true "llm config"
// @Success      200  {object}  httpbase.R
// @Failure      400  {object}  httpbase.R
// @Failure      500  {object}  httpbase.R
// @Router       /admin/ai_search/config [put]
func (h *AISearchConfigHandler) UpdateLLMConfig(ctx *gin.Context) {
	var req types.AISearchLLMConfig
	if err := ctx.ShouldBindJSON(&req); err != nil {
		httpbase.BadRequest(ctx, err.Error())
		return
	}
	if err := h.comp.UpdateLLMConfig(ctx.Request.Context(), req); err != nil {
		httpbase.ServerError(ctx, err)
		return
	}
	httpbase.OK(ctx, nil)
}

// TestAISearchLLMConfig godoc
// @Summary      Test connectivity of the dataset AI search LLM
// @Description  Sends a trivial chat completion; empty fields fall back to the saved/static configuration
// @Tags         Admin
// @Accept       json
// @Produce      json
// @Param        req body types.AISearchLLMTestRequest false "optional overrides"
// @Success      200  {object}  httpbase.R
// @Failure      400  {object}  httpbase.R
// @Router       /admin/ai_search/test [post]
func (h *AISearchConfigHandler) TestLLMConfig(ctx *gin.Context) {
	var req types.AISearchLLMTestRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		httpbase.BadRequest(ctx, err.Error())
		return
	}
	if err := h.comp.TestLLMConfig(ctx.Request.Context(), req); err != nil {
		httpbase.BadRequest(ctx, err.Error())
		return
	}
	httpbase.OK(ctx, nil)
}
