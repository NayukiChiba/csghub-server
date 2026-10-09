package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
)

type fakeAISearchComponent struct {
	events  []types.AIChatEvent
	err     error
	lastReq *types.AIChatRequest
}

func (f *fakeAISearchComponent) Chat(ctx context.Context, req *types.AIChatRequest, emit func(types.AIChatEvent) error) error {
	f.lastReq = req
	if f.err != nil {
		return f.err
	}
	for _, e := range f.events {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

func TestAISearchHandler_DatasetChat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	comp := &fakeAISearchComponent{events: []types.AIChatEvent{
		{Type: types.AIChatEventText, Text: "找到 1 个数据集"},
		{Type: types.AIChatEventDatasets, Datasets: []types.DatasetSummary{{Namespace: "root", Name: "demo"}}},
	}}
	h := NewAISearchHandlerWithComponent(comp)

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ai/dataset_chat",
		strings.NewReader(`{"message":"语音数据集"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.DatasetChat(ctx)

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, `"type":"text"`)
	require.Contains(t, body, `"namespace":"root"`)
	require.Contains(t, body, "data: [DONE]")
	require.Equal(t, "语音数据集", comp.lastReq.Message)
}

func TestAISearchHandler_DatasetChatBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewAISearchHandlerWithComponent(&fakeAISearchComponent{})

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/ai/dataset_chat",
		strings.NewReader(`{"message":""}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.DatasetChat(ctx)
	require.Equal(t, http.StatusBadRequest, w.Code)
}
