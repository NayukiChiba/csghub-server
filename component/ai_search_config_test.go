package component

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	mockllm "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/mcpserver"
)

// fakeAgentConfigStore is an in-memory AgentConfigStore for tests.
type fakeAgentConfigStore struct {
	rows map[string]*database.AgentConfig
}

func newFakeAgentConfigStore() *fakeAgentConfigStore {
	return &fakeAgentConfigStore{rows: map[string]*database.AgentConfig{}}
}

func (f *fakeAgentConfigStore) GetByName(ctx context.Context, name string) (*database.AgentConfig, error) {
	return f.rows[name], nil
}

func (f *fakeAgentConfigStore) GetByID(ctx context.Context, id int64) (*database.AgentConfig, error) {
	for _, row := range f.rows {
		if row.ID == id {
			return row, nil
		}
	}
	return nil, nil
}

func (f *fakeAgentConfigStore) List(ctx context.Context) ([]*database.AgentConfig, error) {
	var out []*database.AgentConfig
	for _, row := range f.rows {
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeAgentConfigStore) Create(ctx context.Context, config *database.AgentConfig) error {
	config.ID = int64(len(f.rows) + 1)
	f.rows[config.Name] = config
	return nil
}

func (f *fakeAgentConfigStore) Update(ctx context.Context, config *database.AgentConfig) error {
	f.rows[config.Name] = config
	return nil
}

func (f *fakeAgentConfigStore) Delete(ctx context.Context, id int64) error {
	for name, row := range f.rows {
		if row.ID == id {
			delete(f.rows, name)
		}
	}
	return nil
}

func (f *fakeAgentConfigStore) GetConfigValue(ctx context.Context, name string, key string) (any, error) {
	row := f.rows[name]
	if row == nil {
		return nil, errors.New("agent config not found")
	}
	return row.Config[key], nil
}

func newAISearchConfigTestComponent(cfg *config.Config, store database.AgentConfigStore, llmClient *mockllm.MockLLMSvcClient) AISearchConfigComponent {
	exec := mcpserver.NewToolExecutor(&aiSearchFakeDatasets{}, &aiSearchFakeRepo{}, &aiSearchFakeTags{}, nil, llmClient, cfg, store)
	return NewAISearchConfigComponentWithDeps(cfg, store, exec, llmClient)
}

func TestAISearchConfig_GetFallsBackToStaticConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://env-llm/v1/chat/completions"
	cfg.AISearch.LLMModel = "env-model"
	comp := newAISearchConfigTestComponent(cfg, newFakeAgentConfigStore(), nil)

	got, err := comp.GetLLMConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "http://env-llm/v1/chat/completions", got.LLMEndpoint)
	require.Equal(t, "env-model", got.LLMModel)
}

func TestAISearchConfig_UpdateOverridesStaticConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://env-llm/v1/chat/completions"
	store := newFakeAgentConfigStore()
	comp := newAISearchConfigTestComponent(cfg, store, nil)

	err := comp.UpdateLLMConfig(context.Background(), types.AISearchLLMConfig{
		LLMEndpoint: "http://db-llm/v1/chat/completions",
		LLMModel:    "db-model",
		LLMAPIKey:   "db-key",
	})
	require.NoError(t, err)

	got, err := comp.GetLLMConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "http://db-llm/v1/chat/completions", got.LLMEndpoint)
	require.Equal(t, "db-model", got.LLMModel)
	require.Equal(t, "db-key", got.LLMAPIKey)

	// updating again rewrites the same row
	err = comp.UpdateLLMConfig(context.Background(), types.AISearchLLMConfig{LLMEndpoint: "http://db-llm2", LLMModel: "m2"})
	require.NoError(t, err)
	require.Len(t, store.rows, 1)
	got, err = comp.GetLLMConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "http://db-llm2", got.LLMEndpoint)
}

func TestAISearchConfig_TestRequiresEndpointAndModel(t *testing.T) {
	comp := newAISearchConfigTestComponent(&config.Config{}, newFakeAgentConfigStore(), nil)
	err := comp.TestLLMConfig(context.Background(), types.AISearchLLMTestRequest{})
	require.ErrorContains(t, err, "endpoint and model")
}

func TestAISearchConfig_TestCallsLLM(t *testing.T) {
	cfg := &config.Config{}
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp := newAISearchConfigTestComponent(cfg, newFakeAgentConfigStore(), llmClient)

	llmClient.EXPECT().Chat(mock.Anything, "http://test-llm/v1/chat/completions", "", map[string]string{"Authorization": "Bearer k"}, mock.Anything).
		Return("pong", nil).Once()

	err := comp.TestLLMConfig(context.Background(), types.AISearchLLMTestRequest{
		LLMEndpoint: "http://test-llm/v1/chat/completions",
		LLMModel:    "m",
		LLMAPIKey:   "k",
	})
	require.NoError(t, err)
}

func TestAISearchConfig_TestPropagatesLLMError(t *testing.T) {
	cfg := &config.Config{}
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp := newAISearchConfigTestComponent(cfg, newFakeAgentConfigStore(), llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return("", errors.New("connection refused")).Once()

	err := comp.TestLLMConfig(context.Background(), types.AISearchLLMTestRequest{
		LLMEndpoint: "http://bad-llm",
		LLMModel:    "m",
	})
	require.ErrorContains(t, err, "connection refused")
}
