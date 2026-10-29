package workflows

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	mockdb "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/builder/parquet"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

func TestConvertedCSVShardsAreCollectedForUpload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	output := filepath.Join(root, "output")
	inputs := []string{}
	for i, content := range []string{"id,text\n1,first\n", "id,other\n2,second\n"} {
		path := filepath.Join(root, fmt.Sprintf("%d.csv", i))
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		inputs = append(inputs, fmt.Sprintf("'%s'", path))
	}
	writer, err := parquet.NewS3Writer(ctx, &config.Config{})
	require.NoError(t, err)
	defer writer.(io.Closer).Close()
	require.NoError(t, writer.ConvertToParquet(ctx, "read_csv_auto", inputs, 1, output))
	paths, err := collectUploadParquetPaths(dvCom.UploadParquetReq{DownloadCard: dvCom.DownloadCard{Subsets: []dvCom.DownloadSubset{{ConfigName: "default", Splits: []dvCom.DownloadSplit{{Name: "train", ExportPath: output}}}}}})
	require.NoError(t, err)
	require.Equal(t, []string{"default/train/00000.parquet", "default/train/00001.parquet"}, paths)
}

func TestUploadRejectsUnrecognizedShardNames(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0-0.parquet"), []byte("not uploaded"), 0600))
	_, err := collectUploadParquetPaths(dvCom.UploadParquetReq{DownloadCard: dvCom.DownloadCard{Subsets: []dvCom.DownloadSubset{{ConfigName: "default", Splits: []dvCom.DownloadSplit{{Name: "train", ExportPath: dir}}}}}})
	require.ErrorContains(t, err, "invalid exported parquet shard name")
}

func TestUnchangedSourcesWithMissingShardsAreRebuilt(t *testing.T) {
	ctx := context.Background()
	card := dvCom.CardData{Configs: []dvCom.ConfigData{{ConfigName: "default"}}}
	for _, tt := range []struct {
		name, metadata string
		rebuild        bool
	}{
		{"missing metadata", "", true},
		{"false zero", `{"rows_num":0,"dataset_info":[{"splits":[{"origins":[{"repo_file":"source.csv"}]}]}]}`, true},
		{"valid empty shard", `{"rows_num":0,"dataset_info":[{"splits":[{"files":[{"repo_file":"0.parquet"}],"origins":[{"repo_file":"source.csv"}]}]}]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := mockdb.NewMockDataviewerStore(t)
			store.EXPECT().GetViewerByRepoID(ctx, int64(1)).Return(&database.Dataviewer{DataviewerJob: &database.DataviewerJob{CardMD5: GetCardDataMD5(card), Status: types.WorkflowDone, CardData: tt.metadata}}, nil)
			act := &dataViewerActivityImpl{viewerStore: store}
			rebuild, err := act.CheckIfNeedRebuild(ctx, dvCom.CheckBuildReq{Req: types.UpdateViewerReq{RepoID: 1}, Card: card})
			require.NoError(t, err)
			require.Equal(t, tt.rebuild, rebuild)
		})
	}
}
