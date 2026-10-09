package component

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/mcpserver"
)

// AISearchConfigComponent manages the admin-editable LLM configuration for
// dataset AI search and tests connectivity against it.
type AISearchConfigComponent interface {
	// GetLLMConfig returns the effective LLM configuration (DB overrides env/toml).
	GetLLMConfig(ctx context.Context) (*types.AISearchLLMConfig, error)
	// UpdateLLMConfig persists the LLM configuration into agent_configs.
	UpdateLLMConfig(ctx context.Context, req types.AISearchLLMConfig) error
	// TestLLMConfig pings the LLM with a trivial chat completion. Empty request
	// fields fall back to the effective configuration.
	TestLLMConfig(ctx context.Context, req types.AISearchLLMTestRequest) error
}

type aiSearchConfigComponentImpl struct {
	cfg   *config.Config
	store database.AgentConfigStore
	exec  *mcpserver.ToolExecutor
	llm   llm.LLMSvcClient
}

func NewAISearchConfigComponent(cfg *config.Config, exec *mcpserver.ToolExecutor) AISearchConfigComponent {
	return &aiSearchConfigComponentImpl{
		cfg:   cfg,
		store: database.NewAgentConfigStore(),
		exec:  exec,
		llm:   llm.NewClient(),
	}
}

// NewAISearchConfigComponentWithDeps builds the component with explicit
// dependencies for testing.
func NewAISearchConfigComponentWithDeps(cfg *config.Config, store database.AgentConfigStore, exec *mcpserver.ToolExecutor, llmClient llm.LLMSvcClient) AISearchConfigComponent {
	return &aiSearchConfigComponentImpl{cfg: cfg, store: store, exec: exec, llm: llmClient}
}

func (c *aiSearchConfigComponentImpl) GetLLMConfig(ctx context.Context) (*types.AISearchLLMConfig, error) {
	endpoint, model, apiKey := c.exec.ResolveLLMConfig(ctx)
	return &types.AISearchLLMConfig{
		LLMEndpoint: endpoint,
		LLMModel:    model,
		LLMAPIKey:   apiKey,
	}, nil
}

func (c *aiSearchConfigComponentImpl) UpdateLLMConfig(ctx context.Context, req types.AISearchLLMConfig) error {
	req.LLMEndpoint = strings.TrimSpace(req.LLMEndpoint)
	req.LLMModel = strings.TrimSpace(req.LLMModel)
	req.LLMAPIKey = strings.TrimSpace(req.LLMAPIKey)

	row, err := c.store.GetByName(ctx, mcpserver.AISearchLLMConfigName)
	if err != nil {
		return fmt.Errorf("get ai search llm config: %w", err)
	}
	value := map[string]any{
		"llm_endpoint": req.LLMEndpoint,
		"llm_model":    req.LLMModel,
		"llm_api_key":  req.LLMAPIKey,
	}
	if row == nil {
		return c.store.Create(ctx, &database.AgentConfig{
			Name:   mcpserver.AISearchLLMConfigName,
			Config: value,
		})
	}
	row.Config = value
	return c.store.Update(ctx, row)
}

func (c *aiSearchConfigComponentImpl) TestLLMConfig(ctx context.Context, req types.AISearchLLMTestRequest) error {
	endpoint, model, apiKey := c.exec.ResolveLLMConfig(ctx)
	if req.LLMEndpoint != "" {
		endpoint = req.LLMEndpoint
	}
	if req.LLMModel != "" {
		model = req.LLMModel
	}
	if req.LLMAPIKey != "" {
		apiKey = req.LLMAPIKey
	}
	if endpoint == "" || model == "" {
		return fmt.Errorf("llm endpoint and model must be configured before testing")
	}

	timeout := time.Duration(c.cfg.AISearch.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	llmCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	headers := map[string]string{}
	if apiKey != "" {
		headers["Authorization"] = "Bearer " + apiKey
	}
	content, err := c.llm.Chat(llmCtx, endpoint, "", headers, types.LLMReqBody{
		Model: model,
		Messages: []types.LLMMessage{
			{Role: "user", Content: "ping"},
		},
	})
	if err != nil {
		slog.Warn("ai search llm test failed", slog.Any("error", err))
		return fmt.Errorf("llm test request failed: %w", err)
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("llm returned an empty response")
	}
	return nil
}
