package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	mockllm "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/dataviewer"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/mcpserver"
)

type aiSearchFakeDatasets struct {
	datasets []*types.Dataset
	total    int
	// lastFilter records the filter of the most recent Index call
	lastFilter *types.RepoFilter
}

func (f *aiSearchFakeDatasets) Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error) {
	f.lastFilter = filter
	return f.datasets, f.total, nil
}

func (f *aiSearchFakeDatasets) Show(ctx context.Context, namespace, name, currentUser string, needOpWeight, needMultiSync bool) (*types.Dataset, error) {
	for _, ds := range f.datasets {
		if ds.Path == namespace+"/"+name {
			return ds, nil
		}
	}
	return nil, errors.New("dataset not found")
}

type aiSearchFakeViewer struct {
	catalog *dataviewer.CatalogResult
	rows    *dataviewer.RowsResult
}

func (f *aiSearchFakeViewer) GetCatalog(ctx context.Context, namespace, name string) (*dataviewer.CatalogResult, error) {
	return f.catalog, nil
}

func (f *aiSearchFakeViewer) GetRows(ctx context.Context, namespace, name, configName, split string, per int) (*dataviewer.RowsResult, error) {
	return f.rows, nil
}

type aiSearchFakeRepo struct{}

func (f *aiSearchFakeRepo) FileRaw(ctx context.Context, req *types.GetFileReq) (string, error) {
	return "", errors.New("not implemented")
}

type aiSearchFakeTags struct {
	tags []*database.Tag
}

func (f *aiSearchFakeTags) AllDatasetTags(ctx context.Context) ([]*database.Tag, error) {
	return f.tags, nil
}

func newAISearchTestComponent(cfg *config.Config, datasets []*types.Dataset, total int, llmClient *mockllm.MockLLMSvcClient) (AISearchComponent, *aiSearchFakeDatasets) {
	ds := &aiSearchFakeDatasets{datasets: datasets, total: total}
	exec := mcpserver.NewToolExecutor(ds, &aiSearchFakeRepo{}, &aiSearchFakeTags{}, nil, nil, llmClient, cfg, nil)
	return NewAISearchComponentWithDeps(cfg, exec), ds
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
	comp, _ := newAISearchTestComponent(cfg, []*types.Dataset{{Path: "root/demo", Name: "demo"}}, 1, nil)

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "demo"})
	require.Len(t, events, 2)
	require.Equal(t, types.AIChatEventText, events[0].Type)
	require.Contains(t, events[0].Text, "AI 解析暂不可用")
	require.Contains(t, events[0].Text, "为您找到 1 个")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Equal(t, "root/demo", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}

func TestAISearchChat_ZeroResults(t *testing.T) {
	cfg := &config.Config{}
	comp, _ := newAISearchTestComponent(cfg, nil, 0, nil)

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "火星土壤样本"})
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "没有找到")
}

func TestAISearchChat_SingleParseThenSearch(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	cfg.AISearch.LLMModel = "test-model"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, llmClient)

	// exactly one LLM call: parse the query into structured filters
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"speech","modality":"audio","language":"chinese","reason":"中文语音数据集"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "有没有中文语音数据集"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "中文语音数据集")
	require.Contains(t, events[0].Text, "为您找到 1 个")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Len(t, events[1].Datasets, 1)

	// parsed filters are forwarded to the search
	require.Equal(t, "speech", ds.lastFilter.Search)
	require.Len(t, ds.lastFilter.SearchGroups, 3)
}

func TestAISearchChat_FallbackWhenLLMFails(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg, nil, 0, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return("", errors.New("llm unavailable")).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "语音"})
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "AI 解析暂不可用")
	// raw message used as keywords
	require.Equal(t, "语音", ds.lastFilter.Search)
}

func TestAISearchChat_HistoryFoldedIntoParseQuery(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, nil, 0, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(req types.LLMReqBody) bool {
		return len(req.Messages) == 2 && req.Messages[1].Role == "user" &&
			containsAll(req.Messages[1].Content, "对话上下文", "图像数据集", "当前问题：那视频的呢？")
	})).Return(`{"keywords":"video","modality":"video"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{
		Message: "那视频的呢？",
		History: []types.AIChatMessage{{Role: "user", Content: "有没有图像数据集"}, {Role: "assistant", Content: "找到这些图像数据集"}},
	})
	require.Len(t, events, 1)
}

func TestAISearchChat_BrowseIntentListsAll(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "root/demo", Name: "demo"}}, 15, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","reason":"user wants to browse all datasets"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "现在有哪些数据集"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "平台目前共有 15 个数据集")
	require.NotContains(t, events[0].Text, "没有找到")
	// no search criteria: plain dataset listing
	require.Empty(t, ds.lastFilter.Search)
	require.Empty(t, ds.lastFilter.SearchGroups)
}

// A misclassified intent=browse that still carries keywords (the LLM does
// this for queries like "视频数据集") must search by those keywords instead
// of listing every dataset on the platform.
func TestAISearchChat_BrowseIntentWithKeywordsStillSearches(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "root/tiny-video", Name: "tiny-video"}}, 1, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"视频数据集"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "视频数据集"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "为您找到 1 个")
	require.NotContains(t, events[0].Text, "平台目前共有")
	// keywords were forwarded to the search
	require.Equal(t, "视频数据集", ds.lastFilter.Search)
	require.NotEmpty(t, ds.lastFilter.SearchGroups)
}

// A browse misclassification over a message that still contains known domain
// terms (e.g. "中文数据集有哪些" -> browse with empty filters) falls back to
// a keyword search over the raw message instead of listing every dataset.
func TestAISearchChat_BrowseMisclassificationWithTermsStillSearches(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}}, 1, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":""}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "中文数据集有哪些"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "为您找到 1 个")
	require.NotContains(t, events[0].Text, "平台目前共有")
	// raw message became the search keywords
	require.Equal(t, "中文数据集有哪些", ds.lastFilter.Search)
	require.NotEmpty(t, ds.lastFilter.SearchGroups)
}

// "现在有哪些数据集" contains no known domain term; browse stays browse.
func TestAISearchChat_PureBrowseNotRescuedByGuard(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "root/demo", Name: "demo"}}, 15, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":""}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "现在有哪些数据集"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "平台目前共有 15 个数据集")
	require.Empty(t, ds.lastFilter.Search)
}

func TestAISearchChat_TagsIntent(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	ds := &aiSearchFakeDatasets{}
	tags := &aiSearchFakeTags{tags: []*database.Tag{
		{Category: "task", Name: "image-classification"},
		{Category: "task", Name: "automatic-speech-recognition"},
		{Category: "license", Name: "mit"},
	}}
	exec := mcpserver.NewToolExecutor(ds, &aiSearchFakeRepo{}, tags, nil, nil, llmClient, cfg, nil)
	comp := NewAISearchComponentWithDeps(cfg, exec)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"tags","keywords":""}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "数据集有哪些标签"})
	// tags are answered as text only; no dataset cards and no search
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "任务类型（共 2 个）")
	require.Contains(t, events[0].Text, "image-classification")
	require.Contains(t, events[0].Text, "许可证（共 1 个）")
	require.Nil(t, ds.lastFilter)
}

func TestAISearchChat_OtherIntentRefused(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg, nil, 0, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"other","keywords":""}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "我喜欢你"})
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "数据集搜索助手")
	// off-topic messages never reach the search
	require.Nil(t, ds.lastFilter)
}

func TestAISearchChat_EmptyMessage(t *testing.T) {
	comp, _ := newAISearchTestComponent(&config.Config{}, nil, 0, nil)
	err := comp.Chat(context.Background(), &types.AIChatRequest{Message: "  "}, func(types.AIChatEvent) error { return nil })
	require.Error(t, err)
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

func TestAISearchChat_DetailIntent(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	ds := &aiSearchFakeDatasets{
		datasets: []*types.Dataset{{
			Path:        "opencompass/mmlu",
			Name:        "mmlu",
			Description: "大规模多任务语言理解评测集",
			License:     "MIT",
			Tags:        []types.RepoTag{{Category: "task", Name: "text-classification"}},
		}},
		total: 1,
	}
	viewer := &aiSearchFakeViewer{
		catalog: &dataviewer.CatalogResult{
			RowsNum: 100,
			DatasetInfos: []dataviewer.DatasetInfoEntry{{
				ConfigName: "default",
				Splits:     []dataviewer.SplitInfo{{Name: "test", NumExamples: 100}},
			}},
		},
		rows: &dataviewer.RowsResult{Columns: []string{"q", "a"}, Rows: [][]any{{"1+1?", "2"}}},
	}
	exec := mcpserver.NewToolExecutor(ds, &aiSearchFakeRepo{}, &aiSearchFakeTags{}, nil, viewer, llmClient, cfg, nil)
	comp := NewAISearchComponentWithDeps(cfg, exec)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"detail","dataset":"mmlu"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "mmlu 这个数据集的基本信息"})
	require.Len(t, events, 2)
	text := events[0].Text
	require.Contains(t, text, "数据集 opencompass/mmlu 的基本信息")
	require.Contains(t, text, "用途：大规模多任务语言理解评测集")
	require.Contains(t, text, "模态：文本")
	require.Contains(t, text, "许可：MIT")
	require.Contains(t, text, "default/test 共 100 行")
	require.Contains(t, text, "q: 1+1?")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Equal(t, "opencompass/mmlu", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}

func TestAISearchChat_DetailIntentNotFound(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	exec := mcpserver.NewToolExecutor(&aiSearchFakeDatasets{}, &aiSearchFakeRepo{}, &aiSearchFakeTags{}, nil, nil, llmClient, cfg, nil)
	comp := NewAISearchComponentWithDeps(cfg, exec)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"detail","dataset":"no-such-dataset"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "no-such-dataset 的用途"})
	require.Len(t, events, 1)
	require.Contains(t, events[0].Text, "没有找到名为「no-such-dataset」的数据集")
}

func TestAISearchChat_DetailIntentWrongNamespaceFallsBackToBareName(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	ds := &aiSearchFakeDatasets{
		datasets: []*types.Dataset{{
			Path:        "csg_opencompass/mmlu",
			Name:        "mmlu",
			Description: "大规模多任务语言理解评测集",
		}},
		total: 1,
	}
	exec := mcpserver.NewToolExecutor(ds, &aiSearchFakeRepo{}, &aiSearchFakeTags{}, nil, nil, llmClient, cfg, nil)
	comp := NewAISearchComponentWithDeps(cfg, exec)

	// the LLM guessed a namespace that does not exist on this instance
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"detail","dataset":"opencompass/mmlu"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "mmlu 这个数据集的基本信息"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "数据集 csg_opencompass/mmlu 的基本信息")
	require.Equal(t, "csg_opencompass/mmlu", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}
