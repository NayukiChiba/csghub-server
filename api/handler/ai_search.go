package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"opencsg.com/csghub-server/api/httpbase"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/component"
)

// AISearchHandler serves the dataset AI chat endpoint.
type AISearchHandler struct {
	comp component.AISearchComponent
}

func NewAISearchHandler(cfg *config.Config) (*AISearchHandler, error) {
	comp, err := component.NewAISearchComponent(cfg)
	if err != nil {
		return nil, fmt.Errorf("create ai search component: %w", err)
	}
	return &AISearchHandler{comp: comp}, nil
}

// NewAISearchHandlerWithComponent builds the handler with an explicit
// component for testing.
func NewAISearchHandlerWithComponent(comp component.AISearchComponent) *AISearchHandler {
	return &AISearchHandler{comp: comp}
}

// DatasetChat godoc
// @Summary      Chat with the dataset search assistant
// @Description  Streams assistant text and structured dataset results as SSE events
// @Tags         Dataset
// @Accept       json
// @Produce      text/event-stream
// @Param        req body types.AIChatRequest true "chat request"
// @Success      200  {string}  string  "SSE stream"
// @Router       /ai/dataset_chat [post]
func (h *AISearchHandler) DatasetChat(ctx *gin.Context) {
	var req types.AIChatRequest
	if err := ctx.ShouldBindJSON(&req); err != nil {
		httpbase.BadRequest(ctx, err.Error())
		return
	}

	ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	ctx.Writer.Header().Set("Cache-Control", "no-cache")
	ctx.Writer.Header().Set("Connection", "keep-alive")
	ctx.Writer.Header().Set("Transfer-Encoding", "chunked")
	ctx.Writer.WriteHeader(http.StatusOK)
	ctx.Writer.Flush()

	emit := func(event types.AIChatEvent) error {
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal chat event: %w", err)
		}
		if _, err := fmt.Fprintf(ctx.Writer, "data: %s\n\n", data); err != nil {
			return err
		}
		ctx.Writer.Flush()
		return nil
	}

	if err := h.comp.Chat(ctx.Request.Context(), &req, emit); err != nil {
		slog.Error("ai dataset chat failed", slog.Any("error", err))
		// headers are already sent; surface the failure as a terminal event
		_ = emit(types.AIChatEvent{Type: types.AIChatEventText, Text: "搜索服务出错了，请稍后再试。"})
	}
	fmt.Fprint(ctx.Writer, "data: [DONE]\n\n")
	ctx.Writer.Flush()
}
