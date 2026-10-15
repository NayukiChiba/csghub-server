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
		LanguageHard: true,
		MaxSizeBytes: &maxSize,
		Sort:         "trending",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Len(t, result.Datasets, 1)
	require.Equal(t, "root", result.Datasets[0].Namespace)
	require.Equal(t, "speech-zh", result.Datasets[0].Name)

	// strict attempt: keyword and modality become AND-combined synonym
	// groups; user-stated chinese maps onto the structured ContentLang
	// column instead of a full-text group; Search keeps the raw keywords
	require.Equal(t, "speech", ds.lastFilter.Search)
	require.False(t, ds.lastFilter.SearchAny)
	require.Len(t, ds.lastFilter.SearchGroups, 2)
	require.Contains(t, ds.lastFilter.SearchGroups[0], "speech")
	require.Contains(t, ds.lastFilter.SearchGroups[1], "音频")
	require.Equal(t, "zh", ds.lastFilter.ContentLang)
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
	require.Len(t, ds.filters, 2)
	require.Len(t, ds.filters[0].SearchGroups, 2)
	require.Len(t, ds.filters[1].SearchGroups, 1)
}

// There is no OR relaxation: the user's criteria are AND-connected by
// intent, so a combination that matches nothing returns an empty result
// instead of dropping a core condition.
func TestSearchDatasets_NoRelaxedFallback(t *testing.T) {
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
	require.Equal(t, 0, result.Total)
	require.Empty(t, result.Datasets)
	for _, filter := range ds.filters {
		require.False(t, filter.SearchAny, "no attempt may relax the AND semantics")
	}
	// strict, aux-dropped; no relaxed attempt
	require.Len(t, ds.filters, 2)
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
	// the tags-only attempt keeps the tag filter in place (a later top-up
	// query may follow it)
	require.NotEmpty(t, ds.filters)
	require.Len(t, ds.filters[0].Tags, 1)
	require.Equal(t, "task", ds.filters[0].Tags[0].Category)
	require.Equal(t, "image-classification", ds.filters[0].Tags[0].Name)
}

// When every strict attempt (text+tag, then tags-only) comes back empty the
// result stays empty: the tag filter is never silently dropped to zero-AND.
// When every strict attempt (text+tag, then text without the tag) comes back
// empty the result stays empty. Crucially the tags-only attempt — which would
// drop the user's content keywords and answer with datasets that merely carry
// an LLM-guessed tag — must never fire while keywords exist.
func TestSearchDatasets_StrictAttemptsExhaustedReturnsEmpty(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			// the tags-only join would match, but must never be consulted
			return []*types.Dataset{{Path: "root/speech-zh", Name: "speech-zh"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "speech",
		Tags:     []TagArg{{Category: "task", Name: "automatic-speech-recognition"}},
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.Total)
	require.Empty(t, result.Datasets)
	// every attempted filter kept the keyword search; none went tags-only
	require.NotEmpty(t, ds.filters)
	for _, filter := range ds.filters {
		require.NotEmpty(t, filter.Search)
	}
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

// When the query carries no content keywords (the LLM expressed everything
// via exact tag filters), a tags-only strict attempt runs before giving up.
func TestSearchDatasets_TagsOnlyAttemptBeforeEmpty(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	var filters []*types.RepoFilter
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		filters = append(filters, filter)
		if filter.Search == "" && len(filter.Tags) > 0 {
			return []*types.Dataset{{Path: "root/img", Name: "img"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Tags: []TagArg{{Category: "license", Name: "mit"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	// the winning attempt is tags-only: no text search, tag filter kept
	require.Empty(t, filters[0].Search)
	require.Empty(t, filters[0].SearchGroups)
	require.Len(t, filters[0].Tags, 1)
	require.Equal(t, "mit", filters[0].Tags[0].Name)
	// the under-filled page is topped up from the tag-name text search
	require.Len(t, filters, 2)
	require.Equal(t, "mit", filters[1].Search)
	require.Empty(t, filters[1].Tags)

	// stopword-only keywords are "no content keywords" too
	filters = nil
	result, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "数据集",
		Tags:     []TagArg{{Category: "license", Name: "mit"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Empty(t, filters[0].Search)
}

// With real content keywords the tags-only attempt must NOT fire: dropping a
// core keyword constraint (多模态评估) and answering with datasets that merely
// carry an LLM-guessed tag is the silent relaxation strict AND forbids.
func TestSearchDatasets_TagsOnlySkippedWhenKeywordsPresent(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			// the tags-only join would match, but must never be consulted
			return []*types.Dataset{{Path: "root/qa", Name: "qa"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "多模态评估",
		Tags:     []TagArg{{Category: "task", Name: "question-answering"}},
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.Total)
	require.Empty(t, result.Datasets)
	// every attempted filter kept the keyword search; none went tags-only
	require.NotEmpty(t, ds.lastFilter.Search)
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
	require.Equal(t, 4, result.Total)
	require.Len(t, result.Datasets, 4)
	require.Equal(t, "ceval-exam", result.Datasets[0].Name)
	require.Equal(t, "scikit-learn-iris", result.Datasets[3].Name)
}

func TestParseDatasetQuery_KeywordExpansionsSanitized(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"多选题","keyword_expansions":[{"term":" 多选题 ","equivalents":["Multiple Choice","multiple choice","多选题","数据集",""]},{"term":"","equivalents":["x"]},{"term":"火星","equivalents":[]}],"reason":"多选题数据集"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "有没有多选题数据集"})
	require.NoError(t, err)
	// trimmed/lowercased, duplicates/stopwords/empty entries dropped
	require.Equal(t, []KeywordExpansion{{Term: "多选题", Equivalents: []string{"multiple choice"}}}, result.KeywordExpansions)
}

func TestSearchDatasets_KeywordExpansionsWidenGroups(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "多选题",
		KeywordExpansions: []KeywordExpansion{
			{Term: "多选题", Equivalents: []string{"multiple choice", "multiple-choice"}},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, ds.lastFilter)
	// the source term and its LLM equivalents form one OR group
	require.Len(t, ds.lastFilter.SearchGroups, 1)
	require.Contains(t, ds.lastFilter.SearchGroups[0], "多选题")
	require.Contains(t, ds.lastFilter.SearchGroups[0], "multiple choice")
	require.Contains(t, ds.lastFilter.SearchGroups[0], "multiple-choice")
}

// Explicit date expressions in the query override the LLM's date fields
// wholesale: the model occasionally misreads compact forms ("10.12" observed
// as 2026-02-12), so dates are parsed deterministically in Go.
func TestParseDatasetQuery_ExplicitDateOverridesLLM(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"","created_after":"2026-02-12","sort":"recently_update"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "2026-10-12之后创建的数据集"})
	require.NoError(t, err)
	require.Equal(t, "2026-10-12", result.CreatedAfter)
	require.Empty(t, result.CreatedBefore)
	require.Empty(t, result.UpdatedAfter)
	require.Empty(t, result.UpdatedBefore)
	// an explicit date outranks the vague recently_update ordering
	require.Empty(t, result.Sort)
}

// Update phrasing routes the deterministic date to the updated bounds.
func TestParseDatasetQuery_ExplicitDateUpdatePhrasing(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"","created_after":"2026-10-12"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "2026-10-12之后更新的数据集"})
	require.NoError(t, err)
	require.Equal(t, "2026-10-12", result.UpdatedAfter)
	require.Empty(t, result.CreatedAfter)
}

// When the query carries no recognizable date expression, the LLM's date
// fields survive untouched.
func TestParseDatasetQuery_FuzzyDateKeepsLLMFields(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"","updated_after":"2026-07-13","sort":"recently_update"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "最近更新的数据集"})
	require.NoError(t, err)
	require.Equal(t, "2026-07-13", result.UpdatedAfter)
	require.Equal(t, "recently_update", result.Sort)
}

func TestSearchDatasetsByDate_MapsDateField(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/demo", Name: "demo"}}
	ds.total = 1

	_, err := exec.SearchDatasetsByDate(context.Background(), SearchDatasetsByDateArgs{
		DateField: "updated",
		After:     "2026-10-12",
		Before:    "2026-10-13",
	})
	require.NoError(t, err)
	require.NotNil(t, ds.lastFilter.UpdatedAfter)
	require.Equal(t, "2026-10-12", ds.lastFilter.UpdatedAfter.Format(time.DateOnly))
	require.NotNil(t, ds.lastFilter.UpdatedBefore)
	require.Nil(t, ds.lastFilter.CreatedAfter)

	_, err = exec.SearchDatasetsByDate(context.Background(), SearchDatasetsByDateArgs{
		DateField: "created",
		After:     "2026-10-01",
	})
	require.NoError(t, err)
	require.NotNil(t, ds.lastFilter.CreatedAfter)
	require.Nil(t, ds.lastFilter.UpdatedAfter)

	_, err = exec.SearchDatasetsByDate(context.Background(), SearchDatasetsByDateArgs{DateField: "modified", After: "2026-10-01"})
	require.Error(t, err)
	_, err = exec.SearchDatasetsByDate(context.Background(), SearchDatasetsByDateArgs{DateField: "created"})
	require.Error(t, err)
}

// Chinese/English language filters become a structured content-language
// constraint (English datasets never say "english" in name/description);
// other languages keep the full-text fallback.
func TestSearchDatasets_LanguageMapsToContentLang(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/iris", Name: "iris"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "文本分类",
		Language: "english", LanguageHard: true,
	})
	require.NoError(t, err)
	require.Equal(t, "en", ds.lastFilter.ContentLang)
	for _, group := range ds.lastFilter.SearchGroups {
		require.NotContains(t, group, "english")
	}

	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Language: "中文", LanguageHard: true})
	require.NoError(t, err)
	require.Equal(t, "zh", ds.lastFilter.ContentLang)

	// no structured equivalent: french stays a full-text group
	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Language: "french", LanguageHard: true})
	require.NoError(t, err)
	require.Empty(t, ds.lastFilter.ContentLang)
	require.Contains(t, ds.lastFilter.SearchGroups, expandToken("french"))
}

// With an explicit size bound, size-descriptor keywords ("small tiny size")
// describe the bound, not the content: they must not become full-text groups
// (小于10kb once answered with only the dataset whose README says "dimension").
func TestSearchDatasets_SizeBoundStripsSizeDescriptorKeywords(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/tiny-video", Name: "tiny-video"}}
	ds.total = 1

	maxSize := int64(10240)
	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "small tiny size",
		KeywordExpansions: []KeywordExpansion{
			{Term: "small", Equivalents: []string{"little", "mini"}},
		},
		MaxSizeBytes: &maxSize,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "", ds.lastFilter.Search)
	require.Empty(t, ds.lastFilter.SearchGroups)
	require.Equal(t, &maxSize, ds.lastFilter.RepoSizeMax)
}

// Keywords that are pure stopwords ("dataset", "数据集") must not become a
// full-text filter of their own: 小于10kb asked with keywords="dataset"
// excluded imagefolder because its empty README never contains the word.
func TestSearchDatasets_StopwordOnlyKeywordsTakePlainPath(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/imagefolder", Name: "imagefolder"}}
	ds.total = 1

	maxSize := int64(10240)
	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords:     "dataset",
		MaxSizeBytes: &maxSize,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "", ds.lastFilter.Search, "stopword-only keywords must not reach the full-text branch")
	require.Empty(t, ds.lastFilter.SearchGroups)
	require.Equal(t, &maxSize, ds.lastFilter.RepoSizeMax)
}

// A language-only query filters by content language on the plain list path.
func TestSearchDatasets_LanguageOnlyUsesContentLang(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/iris", Name: "iris"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{Language: "英文", LanguageHard: true})
	require.NoError(t, err)
	require.Equal(t, "en", ds.lastFilter.ContentLang)
	require.Empty(t, ds.lastFilter.SearchGroups)
}

// Language terms repeated inside keywords must not become full-text groups:
// no real English document contains the word "english".
func TestSearchDatasets_StripsRepeatedLanguageFromKeywords(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/iris", Name: "iris"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "English text classification",
		Language: "english", LanguageHard: true,
	})
	require.NoError(t, err)
	require.Equal(t, "en", ds.lastFilter.ContentLang)
	require.Equal(t, "text classification", ds.lastFilter.Search)
	for _, group := range ds.lastFilter.SearchGroups {
		require.NotContains(t, group, "english")
		require.NotContains(t, group, "en")
	}

	// keywords that are only the language reduce to a content-lang browse
	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "英文",
		Language: "english", LanguageHard: true,
	})
	require.NoError(t, err)
	require.Equal(t, "en", ds.lastFilter.ContentLang)
	require.Empty(t, ds.lastFilter.Search)
	require.Empty(t, ds.lastFilter.SearchGroups)
}

// A language tag from the parser is redundant with the structured
// content-language filter and must not exclude datasets lacking the tag.
func TestSearchDatasets_DropsLanguageTagWhenContentLangSet(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.datasets = []*types.Dataset{{Path: "root/iris", Name: "iris"}}
	ds.total = 1

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "text classification",
		Language: "english", LanguageHard: true,
		Tags: []TagArg{
			{Category: "language", Name: "en"},
			{Category: "task", Name: "text-classification"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "en", ds.lastFilter.ContentLang)
	require.Len(t, ds.lastFilter.Tags, 1)
	require.Equal(t, "text-classification", ds.lastFilter.Tags[0].Name)

	// without a structured language the language tag stays
	_, err = exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "text classification",
		Tags:     []TagArg{{Category: "language", Name: "en"}},
	})
	require.NoError(t, err)
	require.Len(t, ds.lastFilter.Tags, 1)
	require.Equal(t, "language", ds.lastFilter.Tags[0].Category)
}

// LLMs format integer fields inconsistently ("" or "1048576"); the parse
// result must still decode instead of falling back to raw keyword search.
func TestParseDatasetQueryResult_ToleratesStringSizeFields(t *testing.T) {
	var r ParseDatasetQueryResult
	err := json.Unmarshal([]byte(`{"keywords":"text classification","min_size_bytes":"","max_size_bytes":"1048576"}`), &r)
	require.NoError(t, err)
	require.Equal(t, "text classification", r.Keywords)
	require.Nil(t, r.MinSizeBytes)
	require.NotNil(t, r.MaxSizeBytes)
	require.Equal(t, int64(1048576), *r.MaxSizeBytes)

	// plain numbers keep working
	err = json.Unmarshal([]byte(`{"keywords":"x","min_size_bytes":1024}`), &r)
	require.NoError(t, err)
	require.NotNil(t, r.MinSizeBytes)
	require.Equal(t, int64(1024), *r.MinSizeBytes)
}

// An LLM outage must not silently drop the date condition: the deterministic
// date parser runs on the fallback path too.
func TestParseDatasetQuery_FallbackStillAppliesDateConstraint(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return("", errors.New("llm down")).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "10.12之后更新的数据集"})
	require.NoError(t, err)
	require.True(t, result.Fallback)
	require.Equal(t, "2026-10-12", result.UpdatedAfter)
	require.Empty(t, result.CreatedAfter)
}

// Tag coverage is incomplete (HF task_ids never became tags): an under-filled
// tags-only page is topped up from a full-text search on the tag names.
func TestSearchDatasets_TagsOnlyUnderfilledMergesTagNameTextMatches(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			// the tag join finds only the one dataset carrying the tag
			return []*types.Dataset{{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}}, 1, nil
		}
		if filter.Search == "multiple-choice" {
			return []*types.Dataset{
				{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"},
				{Path: "csg_opencompass/mmlu", Name: "mmlu"},
				{Path: "csg_opencompass/ai2_arc", Name: "ai2_arc"},
			}, 3, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Tags: []TagArg{{Category: "task", Name: "multiple-choice"}},
	})
	require.NoError(t, err)
	require.Equal(t, 3, result.Total)
	require.Len(t, result.Datasets, 3)
	require.Equal(t, "csg_opencompass", result.Datasets[0].Namespace)
	require.Equal(t, "ceval-exam", result.Datasets[0].Name)
}

// When the exact tag join finds nothing at all, the tag-name text search is
// the last resort before reporting empty.
func TestSearchDatasets_TagsOnlyEmptyFallsToTagNameText(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "multiple-choice" && len(filter.Tags) == 0 {
			return []*types.Dataset{{Path: "csg_opencompass/mmlu", Name: "mmlu"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Tags: []TagArg{{Category: "task", Name: "multiple-choice"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "mmlu", result.Datasets[0].Name)
}

// A correct-but-sparse tag must not cut away genuine keyword matches: when
// the strict keyword+tag combination finds nothing, the keyword groups are
// retried without the LLM-guessed tag filter.
func TestSearchDatasets_KeywordsRetriedWithoutTags(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	var filters []*types.RepoFilter
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		filters = append(filters, filter)
		if len(filter.Tags) > 0 {
			return nil, 0, nil // the tag join under-recalls
		}
		if filter.Search != "" {
			return []*types.Dataset{{Path: "csg_opencompass/mmlu", Name: "mmlu"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "multiple choice",
		Tags:     []TagArg{{Category: "task", Name: "multiple-choice"}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "mmlu", result.Datasets[0].Name)
	// first attempt kept the tag, the winning one dropped it but kept keywords
	require.Len(t, filters[0].Tags, 1)
	require.Empty(t, filters[len(filters)-1].Tags)
	require.NotEmpty(t, filters[len(filters)-1].Search)
}

// A modality the user typed verbatim (ModalityHard) must survive the whole
// cascade: the keyword-only fallback that drops auxiliary groups is exactly
// what answered "多模态评估" with text-only benchmarks when the LLM kept
// "multimodal" only in the modality field.
func TestSearchDatasets_HardModalitySkipsKeywordOnlyFallback(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "evaluation benchmark", Modality: "multimodal", ModalityHard: true,
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.Total)
	require.NotEmpty(t, ds.filters)
	for _, f := range ds.filters {
		flat := ""
		for _, g := range f.SearchGroups {
			flat += strings.Join(g, " ") + "\n"
		}
		require.Contains(t, flat, "multimodal", "no attempt may drop the hard modality")
	}
}

// The same query with a soft (LLM-inferred) modality keeps the keyword-only
// fallback — it is the recall path for good guesses.
func TestSearchDatasets_SoftModalityKeepsKeywordOnlyFallback(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		return nil, 0, nil
	}

	_, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "evaluation benchmark", Modality: "multimodal",
	})
	require.NoError(t, err)
	var sawKeywordOnly bool
	for _, f := range ds.filters {
		flat := ""
		for _, g := range f.SearchGroups {
			flat += strings.Join(g, " ") + "\n"
		}
		if !strings.Contains(flat, "multimodal") && strings.Contains(flat, "evaluation") {
			sawKeywordOnly = true
		}
	}
	require.True(t, sawKeywordOnly, "soft modality should allow the keyword-only attempt")
}

// When every tag is user-stated (Hard), a tags-only fallback is allowed even
// with keywords present: keyword search cannot reach datasets whose
// name/description/README is empty even though they carry the exact tag.
func TestSearchDatasets_HardTagsAllowTagsOnlyFallback(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.Search == "" && len(filter.Tags) > 0 {
			return []*types.Dataset{{Path: "root/imagefolder", Name: "imagefolder"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "图像分类",
		Tags:     []TagArg{{Category: "task", Name: "image-classification", Hard: true}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "tags", result.MatchMode)
	require.Equal(t, "root/imagefolder", result.Datasets[0].Namespace+"/"+result.Datasets[0].Name)
}

// An LLM-guessed language never filters the primary pass: it is derived from
// the query language, not user intent, and would silently drop matching
// datasets (小于10kb: guessing "chinese" cut English-README datasets under the
// size bound). The first attempt carries no content_lang filter.
func TestSearchDatasets_SoftLanguageSkippedInPrimaryPass(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.ContentLang == "" && len(filter.SearchGroups) > 0 {
			return []*types.Dataset{{Path: "root/librispeech", Name: "librispeech"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "语音 音频", Modality: "audio", ModalityHard: true, Language: "chinese",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	require.Equal(t, "", ds.filters[0].ContentLang, "soft language must not filter the primary pass")
}

// The soft language is still there as a tiebreaker of last resort: when the
// language-free pass matches nothing, the cascade retries with it.
func TestSearchDatasets_SoftLanguageAsLastResort(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.ContentLang == "zh" {
			return []*types.Dataset{{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}}, 1, nil
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "多选题", Language: "chinese",
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Total)
	var sawLang bool
	for _, f := range ds.filters {
		if f.ContentLang == "zh" {
			sawLang = true
		}
	}
	require.True(t, sawLang, "soft language should still be retried when nothing else matches")
}

// A language the user typed verbatim (LanguageHard) is never relaxed away.
func TestSearchDatasets_HardLanguageNeverDropped(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "文本分类", Language: "english", LanguageHard: true,
	})
	require.NoError(t, err)
	require.Equal(t, 0, result.Total)
	require.NotEmpty(t, ds.filters)
	for _, f := range ds.filters {
		require.Equal(t, "en", f.ContentLang, "no attempt may drop the hard language")
	}
}

// A language-relaxed retry tier hits the same tag-coverage wall as the strict
// first attempt: the all-groups retry ANDs the hard tag and only the tag
// carrier survives (1), so the under-filled page must be topped up from the
// tag-less text superset (4). 多选题: ceval-exam carries the multiple-choice
// tag; mmlu/ai2_arc/commonsense_qa only describe it in the README.
func TestSearchDatasets_RetryTierTopsUpUnderfilled(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	carrier := &types.Dataset{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam"}
	textOnly := []*types.Dataset{
		carrier,
		{Path: "csg_opencompass/mmlu", Name: "mmlu"},
		{Path: "csg_opencompass/ai2_arc", Name: "ai2_arc"},
		{Path: "csg_opencompass/commonsense_qa", Name: "commonsense_qa"},
	}
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		if filter.ContentLang == "en" {
			return nil, 0, nil // every candidate is zh; the LLM guessed "english"
		}
		switch {
		case filter.Search == "" && len(filter.Tags) > 0:
			return []*types.Dataset{carrier}, 1, nil // tags-only join
		case len(filter.Tags) > 0:
			return []*types.Dataset{carrier}, 1, nil // text AND tag: carrier only
		case len(filter.SearchGroups) > 0:
			return textOnly, 4, nil // tag-less text superset
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "multiple-choice questions",
		Modality: "text",
		Language: "english",
		Tags:     []TagArg{{Category: "task", Name: "multiple-choice", Hard: true}},
	})
	require.NoError(t, err)
	require.Equal(t, 4, result.Total)
	require.Len(t, result.Datasets, 4)
}

// A deterministic ordering must survive the top-up merge: each query batch is
// internally ordered by SQL, but concatenating batches interleaves them
// (多选题 follow-up "下载量最大的呢": ai2_arc with 1 download landed behind the
// NULL-download tag carriers because it arrived via the tag-less superset).
// The client-side re-sort restores the global order.
func TestSearchDatasets_MostDownloadReordersMergedTopUp(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()
	carrier := &types.Dataset{Path: "csg_opencompass/ceval-exam", Name: "ceval-exam", Downloads: 0}
	textOnly := []*types.Dataset{
		carrier,
		{Path: "csg_opencompass/mmlu", Name: "mmlu", Downloads: 0},
		{Path: "csg_opencompass/ai2_arc", Name: "ai2_arc", Downloads: 1},
		{Path: "csg_opencompass/commonsense_qa", Name: "commonsense_qa", Downloads: 0},
	}
	ds.indexFn = func(filter *types.RepoFilter) ([]*types.Dataset, int, error) {
		switch {
		case filter.Search == "" && len(filter.Tags) > 0:
			return []*types.Dataset{carrier}, 1, nil // tags-only join
		case len(filter.Tags) > 0:
			return []*types.Dataset{carrier}, 1, nil // text AND tag: carrier only
		case len(filter.SearchGroups) > 0:
			return textOnly, 4, nil // tag-less text superset
		}
		return nil, 0, nil
	}

	result, err := exec.SearchDatasets(context.Background(), SearchDatasetsArgs{
		Keywords: "多选题",
		Tags:     []TagArg{{Category: "task", Name: "multiple-choice", Hard: true}},
		Sort:     "most_download",
	})
	require.NoError(t, err)
	require.Equal(t, 4, result.Total)
	require.Len(t, result.Datasets, 4)
	require.Equal(t, "ai2_arc", result.Datasets[0].Name)
}

// Deterministic extraction pins what the user typed even when the LLM
// contradicts or omits it: modality from the message wins over the LLM's
// guess, and a vocabulary tag the user's words fully cover is added as Hard.
func TestParseDatasetQuery_ExplicitConstraintsOverrideLLM(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"image classification","modality":"text"}`, nil).Once()
	tags := &fakeTagLister{tags: []*database.Tag{{Category: "task", Name: "image-classification"}}}
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, tags, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "图像分类数据集"})
	require.NoError(t, err)
	require.False(t, result.Fallback)
	require.Equal(t, "image", result.Modality)
	require.True(t, result.ModalityHard)
	require.Len(t, result.Tags, 1)
	require.Equal(t, "image-classification", result.Tags[0].Name)
	require.True(t, result.Tags[0].Hard)
}

// The LLM-failure fallback keeps the user's explicit constraints: 多模态评估
// stays gated even without a working parser.
func TestParseDatasetQuery_FallbackKeepsExplicitConstraints(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return("", errors.New("llm down")).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: "多模态评估数据集"})
	require.NoError(t, err)
	require.True(t, result.Fallback)
	require.Equal(t, "multimodal", result.Modality)
	require.True(t, result.ModalityHard)
}

// Deterministic extraction runs against the current message (RawQuery), not
// the history-folded query: an earlier turn's modality must not leak into the
// follow-up's constraints.
func TestParseDatasetQuery_ExplicitConstraintsUseRawQuery(t *testing.T) {
	cfg := &config.Config{}
	cfg.AISearch.LLMEndpoint = "http://llm/v1/chat/completions"
	llmClient := mockllm.NewMockLLMSvcClient(t)
	llmClient.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"video"}`, nil).Once()
	exec := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient, cfg, nil)

	ctxQuery := "对话上下文：\n用户：有没有图像数据集\n助手：为您找到 1 个\n当前问题：那视频的呢？"
	result, err := exec.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: ctxQuery, RawQuery: "那视频的呢？"})
	require.NoError(t, err)
	require.Equal(t, "video", result.Modality)

	// without RawQuery the folded history wins (first modality found)
	llmClient2 := mockllm.NewMockLLMSvcClient(t)
	llmClient2.EXPECT().Chat(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(`{"keywords":"video"}`, nil).Once()
	exec2 := NewToolExecutor(&fakeDatasetSearcher{}, &fakeRepoFileReader{}, &fakeTagLister{}, nil, nil, llmClient2, cfg, nil)
	result2, err := exec2.ParseDatasetQuery(context.Background(), ParseDatasetQueryArgs{Query: ctxQuery})
	require.NoError(t, err)
	require.Equal(t, "image", result2.Modality)
}
