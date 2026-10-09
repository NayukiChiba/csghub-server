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

type aiSearchFakeDatasets struct {
	datasets []*types.Dataset
	total    int
}

func (f *aiSearchFakeDatasets) Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error) {
	return f.datasets, f.total, nil
}

func (f *aiSearchFakeDatasets) Show(ctx context.Context, namespace, name, currentUser string, needOpWeight, needMultiSync bool) (*types.Dataset, error) {
	return nil, errors.New("not implemented")
}

type aiSearchFakeRepo struct{}

func (f *aiSearchFakeRepo) FileRaw(ctx context.Context, req *types.GetFileReq) (string, error) {
	return "", errors.New("not implemented")
}

type aiSearchFakeTags struct{}

func (f *aiSearchFakeTags) AllDatasetTags(ctx context.Context) ([]*database.Tag, error) {
	return nil, nil
}

func newAISearchTestComponent(cfg *config.Config, datasets []*types.Dataset, total int, llmClient *mockllm.MockLLMSvcClient) AISearchComponent {
	exec := mcpserver.NewToolExecutor(&aiSearchFakeDatasets{datasets: datasets, total: total},
		&aiSearchFakeRepo{}, &aiSearchFakeTags{}, llmClient, cfg, nil)
	return NewAISearchComponentWithDeps(cfg, exec, llmClient)
}

func collectEvents(t *testing.T, comp AISearchComponent, req *types.AIChatRequest) []types.AIChatEvent {
	t.Helper()
	var events []types.AIChatEvent
	err := comp.Chat(context.Background(), req, func(e types.AIChatEvent) error {
		events = append(events, e)
		return nil
	})
	require.NoError(t, err)
	return events
}

func TestAISearchChat_FallbackWhenLLMNotConfigured(t *testing.T) {
	cfg := &config.Config{}
	comp := newAISearchTestComponent(cfg, []*types.Dataset{{Path: "root/demo", Name: "demo"}}, 1, nil)

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "demo"})
	require.Len(t, events, 2)
	require.Equal(t, types.AIChatEventText, events[0].Type)
	require.Contains(t, events[0].Text, "关键词")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Equal(t, "root/demo", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}

func TestAISearchChat_FallbackZeroResults(t *testing.T) {
	cfg := &config.Config{}
	comp := newAISearchTestComponent(cfg, nil, 0, nil)

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "火星土壤样本"})
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "没有找到")
}

func TestAISearchChat_AgentLoopWithToolCall(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	cfg.AISearch.LLMModel = "test-model"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, llmClient)

	// first round: model requests search_datasets
	llmClient.EXPECT().ChatCompletion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&types.ChatCompletion{Choices: []types.Choice{{Message: types.Message{
			Role: "assistant",
			ToolCalls: []types.LLMToolCall{{
				ID:   "call-1",
				Type: "function",
				Function: types.LLMToolCallFunction{
					Name:      "search_datasets",
					Arguments: `{"keywords":"speech"}`,
				},
			}},
		}}}}, nil).Once()
	// second round: model answers with final text
	llmClient.EXPECT().ChatCompletion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&types.ChatCompletion{Choices: []types.Choice{{Message: types.Message{
			Role:    "assistant",
			Content: "找到一个中文语音数据集：root/speech-zh",
		}}}}, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "有没有中文语音数据集"})
	require.Len(t, events, 2)
	require.Equal(t, types.AIChatEventText, events[0].Type)
	require.Contains(t, events[0].Text, "root/speech-zh")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Len(t, events[1].Datasets, 1)
}

func TestAISearchChat_FallbackWhenLLMFails(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp := newAISearchTestComponent(cfg, nil, 0, llmClient)

	llmClient.EXPECT().ChatCompletion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, errors.New("llm unavailable")).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "语音"})
	require.Len(t, events, 1)
	require.Equal(t, types.AIChatEventText, events[0].Type)
	require.Contains(t, events[0].Text, "关键词")
}

func TestAISearchChat_MaxToolIterTruncation(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	cfg.AISearch.MaxToolIter = 2
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp := newAISearchTestComponent(cfg, nil, 0, llmClient)

	// model keeps requesting tools without ever producing a final answer
	llmClient.EXPECT().ChatCompletion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(&types.ChatCompletion{Choices: []types.Choice{{Message: types.Message{
			Role: "assistant",
			ToolCalls: []types.LLMToolCall{{
				ID:       "call-x",
				Type:     "function",
				Function: types.LLMToolCallFunction{Name: "list_dataset_tags", Arguments: `{}`},
			}},
		}}}}, nil)

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "随便找找"})
	require.Len(t, events, 1)
	require.Equal(t, types.AIChatEventText, events[0].Type)
}

func TestAISearchChat_EmptyMessage(t *testing.T) {
	comp := newAISearchTestComponent(&config.Config{}, nil, 0, nil)
	err := comp.Chat(context.Background(), &types.AIChatRequest{Message: "  "}, func(types.AIChatEvent) error { return nil })
	require.Error(t, err)
}
