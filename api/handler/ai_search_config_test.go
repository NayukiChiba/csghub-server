package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
)

type fakeAISearchConfigComponent struct {
	cfg       *types.AISearchLLMConfig
	updateErr error
	testErr   error
	lastReq   *types.AISearchLLMConfig
}

func (f *fakeAISearchConfigComponent) GetLLMConfig(ctx context.Context) (*types.AISearchLLMConfig, error) {
	return f.cfg, nil
}

func (f *fakeAISearchConfigComponent) UpdateLLMConfig(ctx context.Context, req types.AISearchLLMConfig) error {
	f.lastReq = &req
	return f.updateErr
}

func (f *fakeAISearchConfigComponent) TestLLMConfig(ctx context.Context, req types.AISearchLLMTestRequest) error {
	return f.testErr
}

func newConfigTestContext(method, url, body string) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(method, url, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx, w
}

func TestAISearchConfigHandler_Get(t *testing.T) {
	comp := &fakeAISearchConfigComponent{cfg: &types.AISearchLLMConfig{
		LLMEndpoint: "http://llm/v1/chat/completions",
		LLMModel:    "m",
		LLMAPIKey:   "k",
	}}
	h := NewAISearchConfigHandler(comp)

	ctx, w := newConfigTestContext(http.MethodGet, "/api/v1/admin/ai_search/config", "")
	h.GetLLMConfig(ctx)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"llm_model":"m"`)
}

func TestAISearchConfigHandler_Update(t *testing.T) {
	comp := &fakeAISearchConfigComponent{}
	h := NewAISearchConfigHandler(comp)

	ctx, w := newConfigTestContext(http.MethodPut, "/api/v1/admin/ai_search/config",
		`{"llm_endpoint":"http://llm","llm_model":"m","llm_api_key":"k"}`)
	h.UpdateLLMConfig(ctx)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "m", comp.lastReq.LLMModel)
}

func TestAISearchConfigHandler_TestFailure(t *testing.T) {
	comp := &fakeAISearchConfigComponent{testErr: errors.New("connection refused")}
	h := NewAISearchConfigHandler(comp)

	ctx, w := newConfigTestContext(http.MethodPost, "/api/v1/admin/ai_search/test", `{}`)
	h.TestLLMConfig(ctx)

	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "connection refused")
}

func TestAISearchConfigHandler_UpdateServerError(t *testing.T) {
	comp := &fakeAISearchConfigComponent{updateErr: errors.New("db down")}
	h := NewAISearchConfigHandler(comp)

	ctx, w := newConfigTestContext(http.MethodPut, "/api/v1/admin/ai_search/config",
		`{"llm_endpoint":"http://llm","llm_model":"m"}`)
	h.UpdateLLMConfig(ctx)

	require.Equal(t, http.StatusInternalServerError, w.Code)
}
