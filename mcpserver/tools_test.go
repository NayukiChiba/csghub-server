package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
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
}

func (f *fakeDatasetSearcher) Index(ctx context.Context, filter *types.RepoFilter, per, page int, needOpWeight bool) ([]*types.Dataset, int, error) {
	f.lastFilter = filter
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

	// modality and language augment the keyword string
	require.Equal(t, "speech audio speech voice sound chinese", ds.lastFilter.Search)
	require.Equal(t, &maxSize, ds.lastFilter.RepoSizeMax)
	require.Equal(t, "trending", ds.lastFilter.Sort)
}

func TestSearchDatasets_TagsAndPaginationCaps(t *testing.T) {
	exec, ds, _, _ := newTestExecutor()

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
