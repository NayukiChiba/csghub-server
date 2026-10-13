package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	mockllm "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/llm"
	"opencsg.com/csghub-server/builder/dataviewer"
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
	exec := NewToolExecutor(ds, repo, tags, nil, nil, nil, &config.Config{}, nil)
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
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

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
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

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
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

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
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, tags, nil, nil, llmClient, cfg, nil)

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

type fakeRepoSizeLister struct {
	// sizes maps repository ID to the total size of branch "main"
	sizes map[int64]int64
	err   error
}

func (f *fakeRepoSizeLister) FindByRepositoryIDs(ctx context.Context, repoIDs []int64) ([]*database.RepositoryStatistics, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []*database.RepositoryStatistics
	for _, id := range repoIDs {
		if size, ok := f.sizes[id]; ok {
			out = append(out, &database.RepositoryStatistics{RepositoryID: id, Branch: "main", TotalSize: size})
		}
	}
	return out, nil
}

func TestParseDateBound(t *testing.T) {
	after, err := parseDateBound("2025-06-01", false)
	require.NoError(t, err)
	require.Equal(t, "2025-06-01T00:00:00Z", after.Format(time.RFC3339))

	before, err := parseDateBound("2025-06-01", true)
	require.NoError(t, err)
	// a date-only upper bound covers the whole day
	require.Equal(t, "2025-06-01", before.Format(time.DateOnly))
	require.Equal(t, 23, before.Hour())
	require.Equal(t, 59, before.Minute())

	rfc, err := parseDateBound("2025-06-01T10:30:00Z", false)
	require.NoError(t, err)
	require.Equal(t, 10, rfc.Hour())

	none, err := parseDateBound("", false)
	require.NoError(t, err)
	require.Nil(t, none)

	_, err = parseDateBound("2025-13-40", false)
	require.Error(t, err)
}

func TestSearchDatasets_CreatedDateBounds(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		CreatedAfter:  "2025-06-01",
		CreatedBefore: "2025-12-31",
	})
	require.NoError(t, err)
	require.Equal(t, "2025-06-01", ds.lastFilter.CreatedAfter.Format(time.DateOnly))
	require.Equal(t, "2025-12-31", ds.lastFilter.CreatedBefore.Format(time.DateOnly))
	require.Equal(t, 23, ds.lastFilter.CreatedBefore.Hour())

	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{CreatedAfter: "not-a-date"})
	require.Error(t, err)
}

// Size orderings re-sort the relevance-ranked search window by actual size
// and fill the Size field on every summary.
func TestSearchDatasets_SizeSortFillsSizesAndReorders(t *testing.T) {
	ds := &fakeDatasetSearcher{
		datasets: []*types.Dataset{
			{Path: "root/small", Name: "small", RepositoryID: 1, DefaultBranch: "main"},
			{Path: "root/big", Name: "big", RepositoryID: 2, DefaultBranch: "main"},
			{Path: "root/mid", Name: "mid", RepositoryID: 3, DefaultBranch: "main"},
		},
		total: 3,
	}
	stats := &fakeRepoSizeLister{sizes: map[int64]int64{1: 100, 2: 300, 3: 200}}
	exec := NewToolExecutor(ds, &fakeRepoFileReader{}, &fakeTagLister{}, stats, nil, nil, &config.Config{}, nil)

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Sort: "largest", Per: 2})
	require.NoError(t, err)
	require.Equal(t, 3, result.Total)
	require.Len(t, result.Datasets, 2)
	require.Equal(t, "big", result.Datasets[0].Name)
	require.Equal(t, int64(300), result.Datasets[0].Size)
	require.Equal(t, "mid", result.Datasets[1].Name)
	require.Equal(t, int64(200), result.Datasets[1].Size)

	result, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Sort: "smallest"})
	require.NoError(t, err)
	require.Equal(t, "small", result.Datasets[0].Name)
}

func TestParseDatasetQuery_DropsInvalidSortAndDates(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"intent":"search","keywords":"","sort":"largest","created_after":"2025-01-01","created_before":"2025-13-40","updated_after":"2026-10-01","updated_before":"10-13"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "2025年之后最大的数据集"})
	require.NoError(t, err)
	require.Equal(t, "largest", result.Sort)
	require.Equal(t, "2025-01-01", result.CreatedAfter)
	// an unparseable date must not fail the whole search
	require.Empty(t, result.CreatedBefore)
	require.Equal(t, "2026-10-01", result.UpdatedAfter)
	require.Empty(t, result.UpdatedBefore)
}

type fakeDatasetViewer struct {
	catalog *dataviewer.CatalogResult
	rows    *dataviewer.RowsResult
	catErr  error
	rowsErr error
}

func (f *fakeDatasetViewer) GetCatalog(ctx context.Context, namespace, name string) (*dataviewer.CatalogResult, error) {
	return f.catalog, f.catErr
}

func (f *fakeDatasetViewer) GetRows(ctx context.Context, namespace, name, configName, split string, per int) (*dataviewer.RowsResult, error) {
	return f.rows, f.rowsErr
}

func TestGetDatasetDetail_EnrichesMetadataAndPreview(t *testing.T) {
	ds := &fakeDatasetSearcher{datasets: []*types.Dataset{{
		Path:          "opencompass/mmlu",
		Name:          "mmlu",
		Description:   "Massive Multitask Language Understanding",
		License:       "MIT",
		RepositoryID:  7,
		DefaultBranch: "main",
		CreatedAt:     time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		Tags: []types.RepoTag{
			{Category: "task", Name: "text-classification"},
			{Category: "license", Name: "mit"},
		},
	}}}
	repo := &fakeRepoFileReader{content: "# mmlu readme"}
	stats := &fakeRepoSizeLister{sizes: map[int64]int64{7: 1024}}
	viewer := &fakeDatasetViewer{
		catalog: &dataviewer.CatalogResult{
			RowsNum: 100,
			DatasetInfos: []dataviewer.DatasetInfoEntry{{
				ConfigName: "default",
				Splits:     []dataviewer.SplitInfo{{Name: "test", NumExamples: 100}},
			}},
		},
		rows: &dataviewer.RowsResult{Columns: []string{"q", "a"}, Rows: [][]any{{"1+1?", "2"}}},
	}
	exec := NewToolExecutor(ds, repo, &fakeTagLister{}, stats, viewer, nil, &config.Config{}, nil)

	detail, err := exec.GetDatasetDetail(context.Background(), GetDatasetDetailArgs{Namespace: "opencompass", Name: "mmlu"})
	require.NoError(t, err)
	require.Equal(t, "MIT", detail.License)
	require.Equal(t, []string{"text-classification"}, detail.Tasks)
	require.Equal(t, int64(1024), detail.Size)
	require.Equal(t, int64(100), detail.RowsNum)
	require.Equal(t, []SplitSummary{{Config: "default", Name: "test", Rows: 100}}, detail.Splits)
	require.Equal(t, []string{"q", "a"}, detail.SampleColumns)
	require.Equal(t, [][]any{{"1+1?", "2"}}, detail.SampleRows)
	require.Equal(t, "# mmlu readme", detail.ReadmePreview)
	require.Equal(t, "2024-01-02", detail.CreatedAt)
}

func TestGetDatasetDetail_ViewerFailureIsBestEffort(t *testing.T) {
	ds := &fakeDatasetSearcher{datasets: []*types.Dataset{{
		Path:          "opencompass/mmlu",
		Name:          "mmlu",
		RepositoryID:  7,
		DefaultBranch: "main",
	}}}
	viewer := &fakeDatasetViewer{catErr: errors.New("dataviewer down")}
	exec := NewToolExecutor(ds, &fakeRepoFileReader{}, &fakeTagLister{}, nil, viewer, nil, &config.Config{}, nil)

	detail, err := exec.GetDatasetDetail(context.Background(), GetDatasetDetailArgs{Namespace: "opencompass", Name: "mmlu"})
	require.NoError(t, err)
	require.Empty(t, detail.Splits)
	require.Zero(t, detail.RowsNum)
	require.Empty(t, detail.SampleColumns)
}

func TestSearchDatasets_UpdatedDateBounds(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		UpdatedAfter:  "2026-10-01",
		UpdatedBefore: "2026-10-13",
	})
	require.NoError(t, err)
	require.Equal(t, "2026-10-01", ds.lastFilter.UpdatedAfter.Format(time.DateOnly))
	require.Equal(t, "2026-10-13", ds.lastFilter.UpdatedBefore.Format(time.DateOnly))
	require.Equal(t, 23, ds.lastFilter.UpdatedBefore.Hour())

	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{UpdatedBefore: "not-a-date"})
	require.Error(t, err)
}

// A strict search whose page is under-filled by the LLM-guessed tag join is
// topped up with the same strict text search without the tag filters, so
// datasets that match the text but lack the guessed tag are not cut away.
func TestSearchDatasets_UnderfilledTagSearchMergesTextMatches(t *testing.T) {
	tagged := &types.Dataset{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}
	textOnly := &types.Dataset{Path: "csg_opencompass/ai2_arc", Name: "ai2_arc"}
	ds := &fakeDatasetSearcher{
		indexFn: func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
			if len(filter.Tags) > 0 {
				return []*types.Dataset{tagged}, 1, nil
			}
			return []*types.Dataset{tagged, textOnly}, 2, nil
		},
	}
	exec := NewToolExecutor(ds, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, nil, &config.Config{}, nil)

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "多选题",
		Tags:     []TagArg{{Category: "task", Name: "multiple-choice"}},
		Per:      10,
	})
	require.NoError(t, err)
	require.False(t, result.Relaxed)
	require.Equal(t, 2, result.Total)
	require.Len(t, result.Datasets, 2)
	// tag-backed hit first, then the text-only match
	require.Equal(t, "ceval-exam", result.Datasets[0].Name)
	require.Equal(t, "ai2_arc", result.Datasets[1].Name)
}

// A full strict page needs no top-up: the no-tag query must not run.
func TestSearchDatasets_FullTagSearchDoesNotMerge(t *testing.T) {
	ds := &fakeDatasetSearcher{
		indexFn: func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
			require.NotEmpty(t, filter.Tags, "the no-tag top-up query must not run")
			return []*types.Dataset{
				{Path: "root/a", Name: "a"},
				{Path: "root/b", Name: "b"},
			}, 2, nil
		},
	}
	exec := NewToolExecutor(ds, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, nil, &config.Config{}, nil)

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "demo",
		Tags:     []TagArg{{Category: "task", Name: "multiple-choice"}},
		Per:      2,
	})
	require.NoError(t, err)
	require.Equal(t, 2, result.Total)
	require.Len(t, result.Datasets, 2)
}

// When the text condition adds nothing beyond the tag join (e.g. "文本分类"
// only matches Chinese descriptions), the tags-only superset tops the page up
// with every dataset carrying the tag.
func TestSearchDatasets_UnderfilledTagSearchMergesTagsOnlyMatches(t *testing.T) {
	ceval := &types.Dataset{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}
	ds := &fakeDatasetSearcher{
		indexFn: func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
			switch {
			case filter.Search == "" && len(filter.Tags) > 0:
				// tags-only join: every dataset carrying text-classification
				return []*types.Dataset{ceval,
					{Path: "root/rotten_tomatoes", Name: "rotten_tomatoes"},
					{Path: "root/cornell-movie-review-data-rotten_tomatoes", Name: "cornell-movie-review-data-rotten_tomatoes"},
					{Path: "root/scikit-learn-iris", Name: "scikit-learn-iris"}}, 4, nil
			case len(filter.Tags) > 0:
				return []*types.Dataset{ceval}, 1, nil
			default:
				// text search without tags: only the Chinese description matches
				return []*types.Dataset{ceval}, 1, nil
			}
		},
	}
	exec := NewToolExecutor(ds, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, nil, &config.Config{}, nil)

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "文本分类",
		Tags:     []TagArg{{Category: "task", Name: "text-classification"}},
		Per:      10,
	})
	require.NoError(t, err)
	require.False(t, result.Relaxed)
	require.Equal(t, 4, result.Total)
	require.Len(t, result.Datasets, 4)
	require.Equal(t, "ceval-exam", result.Datasets[0].Name)
	require.Equal(t, "scikit-learn-iris", result.Datasets[3].Name)
}
