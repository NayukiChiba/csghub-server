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
	// indexFn, when set, overrides the static datasets/total response
	indexFn func(filter *types.RepoFilter) ([]*types.Dataset, int, error)
}

func (f *aiSearchFakeDatasets) Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error) {
	f.lastFilter = filter
	if f.indexFn != nil {
		return f.indexFn(filter)
	}
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
	// the failing AND-combination is spelled out, not a generic message
	require.Contains(t, events[0].Text, "关键词「火星土壤样本」")
	require.Contains(t, events[0].Text, "组合在平台上不存在")
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

	// parsed filters are forwarded to the search; chinese maps onto the
	// structured ContentLang column instead of a full-text group
	require.Equal(t, "speech", ds.lastFilter.Search)
	require.Len(t, ds.lastFilter.SearchGroups, 2)
	require.Equal(t, "zh", ds.lastFilter.ContentLang)
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
// terms (e.g. "中文数据集有哪些" -> browse with empty filters) must not list
// every dataset: deterministic extraction pins the content-language filter
// the user stated, so the search is scoped to Chinese datasets even though
// the LLM produced no criteria.
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
	// the user's "中文" became the structured language filter
	require.Equal(t, "zh", ds.lastFilter.ContentLang)
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
	require.Contains(t, text, "### 数据集 opencompass/mmlu")
	require.Contains(t, text, "**用途**：大规模多任务语言理解评测集")
	require.Contains(t, text, "- **模态**：文本")
	require.Contains(t, text, "- **许可**：MIT")
	require.Contains(t, text, "default/test 共 100 行")
	// sample rows render as a markdown table under the attribute table
	require.Contains(t, text, "**样例数据**")
	require.Contains(t, text, "| q | a |")
	require.Contains(t, text, "| 1+1? | 2 |")
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
	require.Contains(t, events[0].Text, "### 数据集 csg_opencompass/mmlu")
	require.Equal(t, "csg_opencompass/mmlu", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}

// The LLM's keyword expansions must flow from parse into the search filter so
// a Chinese query can hit English dataset docs.
func TestAISearchChat_KeywordExpansionsPassThrough(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, ds := newAISearchTestComponent(cfg,
		[]*types.Dataset{{Path: "opencompass/mmlu", Name: "mmlu"}}, 1, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"多选题","keyword_expansions":[{"term":"多选题","equivalents":["multiple choice","multiple-choice"]}],"reason":"多选题数据集"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "有没有多选题数据集"})
	require.Len(t, events, 2)
	require.NotNil(t, ds.lastFilter)
	require.Len(t, ds.lastFilter.SearchGroups, 1)
	require.Contains(t, ds.lastFilter.SearchGroups[0], "多选题")
	require.Contains(t, ds.lastFilter.SearchGroups[0], "multiple choice")
}

// When keyword search cannot reach a dataset that carries the exact tag the
// user asked for (empty name/description/README), the tag-only fallback must
// fire AND the answer must say the match came from tags, not content.
func TestAISearchChat_TagOnlyMatchIsLabeled(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	ds := &aiSearchFakeDatasets{}
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			return []*types.Dataset{{Path: "root/imagefolder", Name: "imagefolder"}}, 1, nil
		}
		return nil, 0, nil
	}
	tags := &aiSearchFakeTags{tags: []*database.Tag{{Category: "task", Name: "image-classification"}}}
	exec := mcpserver.NewToolExecutor(ds, &aiSearchFakeRepo{}, tags, nil, nil, llmClient, cfg, nil)
	comp := NewAISearchComponentWithDeps(cfg, exec)

	// the LLM forgets the tag; deterministic extraction adds it (hard)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"search","keywords":"图像分类","reason":"图像分类数据集"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "图像分类数据集"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "为您找到 1 个")
	require.Contains(t, events[0].Text, "按数据集标签匹配")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Equal(t, "root/imagefolder", events[1].Datasets[0].Namespace+"/"+events[1].Datasets[0].Name)
}

func TestHistoryUserMessages(t *testing.T) {
	history := []types.AIChatMessage{
		{Role: "user", Content: "小于10kb的数据集有哪些"},
		{Role: "assistant", Content: "为您找到 2 个…"},
		{Role: "user", Content: "  "},
		{Role: "system", Content: "ignored"},
		{Role: "user", Content: "最大的是谁"},
	}
	require.Equal(t, []string{"小于10kb的数据集有哪些", "最大的是谁"}, historyUserMessages(history))
	require.Nil(t, historyUserMessages(nil))
}

// A same-day date window reads as one day, not as a confusing "之后 + 之前"
// pair ("今天创建的数据集" produced both bounds at the same date).
func TestDescribeCriteria_SameDayWindowCollapses(t *testing.T) {
	p := &mcpserver.ParseDatasetQueryResult{CreatedAfter: "2026-10-15", CreatedBefore: "2026-10-15"}
	require.Equal(t, "创建于 2026-10-15 当天", describeCriteria(p))

	p = &mcpserver.ParseDatasetQueryResult{UpdatedAfter: "2026-10-15", UpdatedBefore: "2026-10-15"}
	require.Equal(t, "更新于 2026-10-15 当天", describeCriteria(p))

	p = &mcpserver.ParseDatasetQueryResult{CreatedAfter: "2026-10-01", CreatedBefore: "2026-10-15"}
	require.Equal(t, "创建于 2026-10-01 之后 + 创建于 2026-10-15 之前", describeCriteria(p))
}

func TestSortSuffix_CreationTimeSorts(t *testing.T) {
	require.Equal(t, "（按创建时间从早到晚排序）", sortSuffix("oldest"))
	require.Equal(t, "（按创建时间从晚到早排序）", sortSuffix("newest"))
}

func TestAISearchChat_ExtremumReturnsTopAndTies(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, []*types.Dataset{
		{Path: "root/a", Name: "a", Downloads: 1},
		{Path: "root/b", Name: "b", Downloads: 5},
		{Path: "root/c", Name: "c", Downloads: 5},
	}, 3, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","sort":"most_download"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "下载最多的数据集是哪个？"})
	require.Len(t, events, 2)
	// the tie block answers the question, not the whole ranking
	require.Contains(t, events[0].Text, "下载量最高的有 2 个数据集，均为 5 次下载")
	require.Equal(t, types.AIChatEventDatasets, events[1].Type)
	require.Len(t, events[1].Datasets, 2)
	names := []string{events[1].Datasets[0].Name, events[1].Datasets[1].Name}
	require.ElementsMatch(t, []string{"b", "c"}, names)
}

func TestAISearchChat_ExtremumSingleWinner(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, []*types.Dataset{
		{Path: "root/a", Name: "a", Downloads: 1},
		{Path: "root/b", Name: "b", Downloads: 9},
	}, 2, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","sort":"most_download"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "哪个数据集下载量最高？"})
	require.Len(t, events, 2)
	require.Contains(t, events[0].Text, "下载量最高的是 **root/b**（9 次下载）。")
	require.Len(t, events[1].Datasets, 1)
	require.Equal(t, "b", events[1].Datasets[0].Name)
}

func TestExtremumTop(t *testing.T) {
	ds := []types.DatasetSummary{
		{Namespace: "root", Name: "a", Downloads: 7, CreatedAt: "2026-10-08"},
		{Namespace: "root", Name: "b", Downloads: 7, CreatedAt: "2026-10-09"},
		{Namespace: "root", Name: "c", Downloads: 3, CreatedAt: "2026-10-09"},
	}
	// ties on the sort key stay; the rest is cut
	require.Len(t, extremumTop(ds, "most_download"), 2)
	// different key, different tie block
	require.Len(t, extremumTop(ds[1:], "oldest"), 2)
	// sorts without a comparable summary value answer the top entry alone
	require.Len(t, extremumTop(ds, "trending"), 1)
	require.Len(t, extremumTop(ds, "most_star"), 1)
	// degenerate windows pass through
	require.Empty(t, extremumTop(nil, "most_download"))
	require.Len(t, extremumTop(ds[:1], "most_download"), 1)
}

func TestAISearchChat_OutputCapLimitsCards(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	cfg.AISearch.ChatMaxResults = 2
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, []*types.Dataset{
		{Path: "root/a", Name: "a"},
		{Path: "root/b", Name: "b"},
		{Path: "root/c", Name: "c"},
	}, 3, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"demo"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{Message: "demo"})
	require.Len(t, events, 2)
	// the over-cap answer names the total, shows only the capped cards, and
	// points at narrowing/other routes instead of listing everything
	require.Contains(t, events[0].Text, "为您找到 3 个相关数据集")
	require.Contains(t, events[0].Text, "单次最多展示 2 个")
	require.Contains(t, events[0].Text, "缩小范围")
	require.Len(t, events[1].Datasets, 2)
}

func TestComposeChatAnswer_OverflowHint(t *testing.T) {
	// browse over the cap: total + hint
	browse := &mcpserver.ParseDatasetQueryResult{Sort: "most_download"}
	out := composeChatAnswer(browse, &mcpserver.SearchDatasetsResult{Total: 15}, 10)
	require.Contains(t, out, "平台目前共有 15 个数据集")
	require.Contains(t, out, "单次最多展示 10 个")

	// under the cap: no hint
	out = composeChatAnswer(browse, &mcpserver.SearchDatasetsResult{Total: 5}, 10)
	require.NotContains(t, out, "单次最多展示")

	// zero results never carry the hint
	out = composeChatAnswer(&mcpserver.ParseDatasetQueryResult{Keywords: "x"}, &mcpserver.SearchDatasetsResult{}, 10)
	require.NotContains(t, out, "单次最多展示")
}

func TestChatMaxResultsClamp(t *testing.T) {
	comp := &aiSearchComponentImpl{cfg: &config.Config{}}
	require.Equal(t, 10, comp.chatMaxResults()) // zero config falls back to default
	comp.cfg.AISearch.ChatMaxResults = 3
	require.Equal(t, 3, comp.chatMaxResults())
	comp.cfg.AISearch.ChatMaxResults = 500
	require.Equal(t, mcpserver.MaxPerPage, comp.chatMaxResults()) // never above the search window
}

func TestAISearchChat_ZeroResultFollowUpClarifies(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, nil, 0, llmClient)

	// the LLM folds the whole history and resurrects the superseded
	// turn-1 topic plus a hallucinated tag — the AND search finds nothing
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"multimodal multiple choice","tags":[{"category":"task","name":"multiple-choice"}],"modality":"multimodal","language":"english"}`, nil).Once()

	events := collectEvents(t, comp, &types.AIChatRequest{
		Message: "这里面哪个数据规模最大？",
		History: []types.AIChatMessage{
			{Role: "user", Content: "我想要用于多模态的数据集"},
			{Role: "assistant", Content: "没有找到符合条件的数据集"},
			{Role: "user", Content: "好吧，有多选题数据吗？"},
			{Role: "assistant", Content: "为您找到 4 个相关数据集"},
		},
	})
	// instead of blaming the scope combination, the chat asks which scope
	// was meant, with clickable self-contained options
	require.Len(t, events, 1)
	require.Equal(t, types.AIChatEventQuestion, events[0].Type)
	require.NotNil(t, events[0].Question)
	require.Len(t, events[0].Question.Options, 2)
	require.Equal(t, "多选题数据集中，这里面哪个数据规模最大？", events[0].Question.Options[0].Send)
	require.Equal(t, "所有数据集中，这里面哪个数据规模最大？", events[0].Question.Options[1].Send)
}

func TestAISearchChat_ZeroResultWithHardConstraintDoesNotClarify(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	comp, _ := newAISearchTestComponent(cfg, nil, 0, llmClient)

	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"multiple choice","language":"spanish"}`, nil).Once()

	// 西班牙语 is the user's own word: the zero is trustworthy, no question
	events := collectEvents(t, comp, &types.AIChatRequest{
		Message: "它们里面有西班牙语的吗",
		History: []types.AIChatMessage{{Role: "user", Content: "有多选题数据吗"}},
	})
	require.Len(t, events, 1)
	require.Equal(t, types.AIChatEventText, events[0].Type)
	require.Contains(t, events[0].Text, "没有找到")
}

// Sample tables must stay valid markdown: pipes in the data are escaped and
// ragged rows are normalized to the column count.
func TestFormatSampleTable(t *testing.T) {
	out := formatSampleTable([]string{"q", "a"}, [][]any{
		{"a|b", "x"},
		{"only-one"},
		{"1", "2", "extra-dropped"},
	})
	require.Contains(t, out, "| q | a |\n| --- | --- |\n")
	require.Contains(t, out, "a\\|b")
	require.Contains(t, out, "| only-one |  |")
	require.Contains(t, out, "| 1 | 2 |")
	require.NotContains(t, out, "extra-dropped")
}
