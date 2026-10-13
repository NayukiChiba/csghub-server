package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	mockllm "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
)

type fakeDatasetSearcher struct {
	datasets []*types.Dataset
	total    int
	err      error
	// lastFilter records the filter of the most recent Index call
	lastFilter *types.RepoFilter
	// filters records every Index call, for cascade tests
	filters []*types.RepoFilter
	// indexFn, when set, overrides the static datasets/total response
	indexFn func(filter *types.RepoFilter) ([]*types.Dataset, int, error)
}

func (f *fakeDatasetSearcher) Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error) {
	f.lastFilter = filter
	f.filters = append(f.filters, filter)
	if f.indexFn != nil {
		return f.indexFn(filter)
	}
	return f.datasets, f.total, f.err
}

func (f *fakeDatasetSearcher) Show(ctx context.Context, namespace, name, currentUser string, needOpWeight, needMultiSync bool) (*types.Dataset, error) {
	for _, ds := range f.datasets {
		if ds.Path == namespace+"/"+name {
			return ds, nil
		}
	}
	return nil, errors.New("dataset not found")
}

type fakeRepoFileReader struct {
	content string
	err     error
	lastReq *types.GetFileReq
}

func (f *fakeRepoFileReader) FileRaw(ctx context.Context, req *types.GetFileReq) (string, error) {
	f.lastReq = req
	return f.content, f.err
}

type fakeTagLister struct {
	tags []*database.Tag
	err  error
}

func (f *fakeTagLister) AllDatasetTags(ctx context.Context) ([]*database.Tag, error) {
	return f.tags, f.err
}

func newTestExecutor() (*ToolExecutor, *fakeDatasetSearcher, *fakeRepoFileReader, *fakeTagLister) {
	ds := &fakeDatasetSearcher{}
	repo := &fakeRepoFileReader{}
	tags := &fakeTagLister{}
	exec := NewToolExecutor(ds, repo, tags, nil, &config.Config{}, nil)
	return exec, ds, repo, tags
}

func TestSearchDatasets_BuildsFilter(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{
		Path:        "root/speech-zh",
		Name:        "speech-zh",
		Description: "Chinese speech dataset",
		Tags:        []types.RepoTag{{Name: "automatic-speech-recognition"}},
	}}
	ds.total = 1
	maxSize := int64(50 * 1024 * 1024)

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords:     "speech",
		Modality:     "audio",
		Language:     "chinese",
		MaxSizeBytes: &maxSize,
		Sort:         "trending",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Len(t, result.Datasets, 1)
	require.Equal(t, "root", result.Datasets[0].Namespace)
	require.Equal(t, "speech-zh", result.Datasets[0].Name)
	require.False(t, result.Relaxed)

	// strict attempt: keyword, modality and language become AND-combined
	// synonym groups; Search keeps the raw keywords
	require.Equal(t, "speech", ds.lastFilter.Search)
	require.False(t, ds.lastFilter.SearchAny)
	require.Len(t, ds.lastFilter.SearchGroups, 3)
	require.Contains(t, ds.lastFilter.SearchGroups[0], "speech")
	require.Contains(t, ds.lastFilter.SearchGroups[1], "音频")
	require.Contains(t, ds.lastFilter.SearchGroups[2], "中文")
	require.Equal(t, &maxSize, ds.lastFilter.RepoSizeMax)
	require.Equal(t, "trending", ds.lastFilter.Sort)
}

func TestSearchDatasets_CascadeDropsAuxGroups(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	// strict with modality finds nothing; keyword-only strict hits
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if len(filter.SearchGroups) == 1 {
			return []*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "speech",
		Modality: "audio",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.False(t, result.Relaxed)
	require.Len(t, ds.filters, 2)
	require.Len(t, ds.filters[0].SearchGroups, 2)
	require.Len(t, ds.filters[1].SearchGroups, 1)
}

func TestSearchDatasets_RelaxedFallback(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.SearchAny {
			return []*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "speech",
		Modality: "audio",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.True(t, result.Relaxed)
	require.True(t, ds.lastFilter.SearchAny)
	// strict, aux-dropped, relaxed
	require.Len(t, ds.filters, 3)
}

func TestSearchDatasets_ModalityOnlySynthesizesSearch(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Modality: "image"})
	require.NoError(t, err)
	// the store layer's full-text branch requires a non-empty Search
	require.NotEmpty(t, ds.lastFilter.Search)
	require.Len(t, ds.lastFilter.SearchGroups, 1)
	require.Contains(t, ds.lastFilter.SearchGroups[0], "图像")
}

func TestSearchDatasets_TagsAndPaginationCaps(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	// a hit on the first attempt keeps the tag filter in place
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Tags: []TagArg{{Category: "task", Name: "image-classification"}},
		Per:  500,
		Page: -1,
	})
	require.NoError(t, err)
	require.Len(t, ds.lastFilter.Tags, 1)
	require.Equal(t, "task", ds.lastFilter.Tags[0].Category)
	require.Equal(t, "image-classification", ds.lastFilter.Tags[0].Name)
}

func TestSearchDatasets_TagFilterDroppedAsLastResort(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	// tag-filtered attempts find nothing; the tagless relaxed attempt hits
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if len(filter.Tags) > 0 {
			return nil, 0, nil
		}
		return []*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "speech",
		Tags:     []TagArg{{Category: "task", Name: "automatic-speech-recognition"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.True(t, result.Relaxed)
	require.Empty(t, ds.lastFilter.Tags)
}

func TestListDatasetTags_FilterByCategory(t *testing.T) {
	exec, _, _, tags := newTestExecutor()
	tags.tags = []*database.Tag{
		{Category: "task", Name: "image-classification"},
		{Category: "license", Name: "mit"},
	}

	all, err := exec.ListDatasetTags(context.Background(), ListDatasetTagsArgs{})
	require.NoError(t, err)
	require.Len(t, all.Tags, 2)

	taskOnly, err := exec.ListDatasetTags(context.Background(), ListDatasetTagsArgs{Category: "task"})
	require.NoError(t, err)
	require.Len(t, taskOnly.Tags, 1)
	require.Equal(t, "image-classification", taskOnly.Tags[0].Name)
}

func TestGetDatasetDetail_WithReadme(t *testing.T) {
	exec, ds, repo, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	repo.content = "# demo dataset"

	detail, err := exec.GetDatasetDetail(context.Background(), GetDatasetDetailArgs{Namespace: "root", Name: "demo"})
	require.NoError(t, err)
	require.Equal(t, "root", detail.Namespace)
	require.Equal(t, "# demo dataset", detail.ReadmePreview)
	require.Equal(t, types.ReadmeFileName, repo.lastReq.Path)
	require.Equal(t, types.DatasetRepo, repo.lastReq.RepoType)
}

func TestGetDatasetDetail_ReadmeMissingIsNotFatal(t *testing.T) {
	exec, ds, repo, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	repo.err = errors.New("file not found")

	detail, err := exec.GetDatasetDetail(context.Background(), GetDatasetDetailArgs{Namespace: "root", Name: "demo"})
	require.NoError(t, err)
	require.Empty(t, detail.ReadmePreview)
}

func TestGetDatasetDetail_RequiresNamespaceAndName(t *testing.T) {
	exec, _, _, _ := newTestExecutor()
	_, err := exec.GetDatasetDetail(context.Background(), GetDatasetDetailArgs{Namespace: "root"})
	require.Error(t, err)
}

func TestParseDatasetQuery_FallbackWhenLLMNotConfigured(t *testing.T) {
	exec, _, _, _ := newTestExecutor()
	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "中文语音数据集"})
	require.NoError(t, err)
	require.True(t, result.Fallback)
	require.Equal(t, "中文语音数据集", result.Keywords)
}

func TestParseDatasetQuery_BrowseIntentKeepsEmptyKeywords(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","reason":"user wants to browse all datasets"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "现在有哪些数据集"})
	require.NoError(t, err)
	require.False(t, result.Fallback)
	require.Equal(t, "browse", result.Intent)
	// empty keywords is a valid browse intent, not forced back to the raw query
	require.Empty(t, result.Keywords)
}

func TestParseDatasetQuery_ZeroSizeBoundsAreDropped(t *testing.T) {
	// LLMs often echo the schema's numeric fields as explicit 0; a zero max
	// size would filter out every dataset.
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","min_size_bytes":0,"max_size_bytes":0}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "现在有哪些数据集"})
	require.NoError(t, err)
	require.Nil(t, result.MinSizeBytes)
	require.Nil(t, result.MaxSizeBytes)
}

func TestParseDatasetQuery_SortWhitelist(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","sort":"most_download"}`, nil).Once()
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"browse","keywords":"","sort":"download_count desc"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, llmClient, cfg, nil)

	valid, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "下载最多的数据集"})
	require.NoError(t, err)
	require.Equal(t, "most_download", valid.Sort)

	invalid, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "下载最多的数据集"})
	require.NoError(t, err)
	require.Empty(t, invalid.Sort)
}

func TestSearchDatasets_ZeroSizeBoundsAreIgnored(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	zero := int64(0)
	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		MinSizeBytes: &zero,
		MaxSizeBytes: &zero,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Nil(t, ds.lastFilter.RepoSizeMin)
	require.Nil(t, ds.lastFilter.RepoSizeMax)
}

// When the LLM produced exact tag filters, a tags-only strict attempt runs
// before the relaxed OR fallback: keywords are the least reliable channel
// (e.g. "许可证" matches nothing in name/description), while tags come from
// the real vocabulary.
func TestSearchDatasets_TagsOnlyAttemptBeforeRelaxed(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			return []*types.Dataset{{Path: "root/img", Name: "img"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "MIT 许可证 图像",
		Modality: "image",
		Tags:     []TagArg{{Category: "license", Name: "mit"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.False(t, result.Relaxed)
	// the winning attempt is tags-only: no text search, tag filter kept
	require.Empty(t, ds.lastFilter.Search)
	require.Empty(t, ds.lastFilter.SearchGroups)
	require.Len(t, ds.lastFilter.Tags, 1)
	require.Equal(t, "mit", ds.lastFilter.Tags[0].Name)
}

// Keywords-only queries must not trigger the tags-only attempt.
func TestSearchDatasets_NoTagsSkipsTagsOnlyAttempt(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	var searches []string
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		searches = append(searches, filter.Search)
		return nil, 0, nil
	}

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Keywords: "demo"})
	require.NoError(t, err)
	for _, s := range searches {
		require.Equal(t, "demo", s)
	}
}

func TestParseTagVocab(t *testing.T) {
	vocab := parseTagVocab([]*database.Tag{
		{Category: "task", Name: "text-classification"},
		{Category: "task", Name: "image-classification"},
		{Category: "license", Name: "mit"},
		{Category: "language", Name: "zh"},
		{Category: "framework", Name: "pytorch"}, // not offered to the LLM
	})
	require.Contains(t, vocab, "task: text-classification, image-classification")
	require.Contains(t, vocab, "license: mit")
	require.Contains(t, vocab, "language: zh")
	require.NotContains(t, vocab, "pytorch")
	require.Empty(t, parseTagVocab(nil))
}

// The parse prompt must carry the real tag vocabulary so the LLM can emit
// exact tag filters.
func TestParseDatasetQuery_PromptContainsTagVocab(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(req types.LLMReqBody) bool {
		return strings.Contains(req.Messages[0].Content, "text-classification") &&
			strings.Contains(req.Messages[0].Content, "language: zh")
	})).Return(`{"intent":"search","language":"chinese","tags":[{"category":"language","name":"zh"}],"keywords":""}`, nil).Once()
	tags := &fakeTagLister{tags: []*database.Tag{
		{Category: "task", Name: "text-classification"},
		{Category: "language", Name: "zh"},
	}}
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, tags, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "中文数据集有哪些"})
	require.NoError(t, err)
	require.Equal(t, "chinese", result.Language)
	require.Len(t, result.Tags, 1)
}

func TestExecute_Dispatch(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	out, err := exec.Execute(context.Background(), "search_datasets", `{"keywords":"demo"}`)
	require.NoError(t, err)
	var result SearchDatasetsResult
	require.NoError(t, json.Unmarshal([]byte(out), &result))
	require.Equal(t, 1, result.Total)

	_, err = exec.Execute(context.Background(), "no_such_tool", `{}`)
	require.Error(t, err)
}
