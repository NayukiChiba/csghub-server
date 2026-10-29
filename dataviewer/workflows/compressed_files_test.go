package workflows

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.temporal.io/sdk/testsuite"
	mockGit "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/git/gitserver"
	"opencsg.com/csghub-server/builder/git/gitserver"
	"opencsg.com/csghub-server/builder/parquet"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

func compressedFixture(t *testing.T, format, compression, content string) *dvCom.FileObject {
	t.Helper()
	var buf bytes.Buffer
	var writer io.WriteCloser
	if compression == ".gz" {
		writer = gzip.NewWriter(&buf)
	} else {
		w, err := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
		require.NoError(t, err)
		writer = w
	}
	_, err := io.WriteString(writer, content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	file := &dvCom.FileObject{RepoFile: "train" + format + compression, LocalRepoPath: t.TempDir(), LocalFileName: "00000" + format + compression, Size: int64(buf.Len()), DownloadSize: int64(buf.Len())}
	require.NoError(t, os.WriteFile(filepath.Join(file.LocalRepoPath, file.LocalFileName), buf.Bytes(), 0o600))
	return file
}

func TestCompressedDatasetClassification(t *testing.T) {
	fc := &dvCom.RepoFilesClass{AllFiles: map[string]*dvCom.RepoFile{}, JsonlFiles: map[string]*dvCom.RepoFile{}, CsvFiles: map[string]*dvCom.RepoFile{}}
	for _, name := range []string{"train.jsonl.gz", "TRAIN.JSON.ZST", "test.csv.gz", "data.tar.gz", "file.gz", "data.jsonl.gz.gz"} {
		appendFile(&types.File{Name: name, Path: name, Size: 10}, fc, 1024)
	}
	require.Len(t, fc.JsonlFiles, 2)
	require.Len(t, fc.CsvFiles, 1)
	require.Len(t, fc.AllFiles, 3)
}

func TestCompressedDatasetConversion(t *testing.T) {
	for _, compression := range []string{".gz", ".zst"} {
		for _, format := range []string{".jsonl", ".json", ".csv"} {
			t.Run(format+compression, func(t *testing.T) {
				content := map[string]string{".jsonl": "{\"id\":1}\n{\"id\":2}\n", ".json": `[{"id":1},{"id":2}]`, ".csv": "id\n1\n2\n"}[format]
				file := compressedFixture(t, format, compression, "\xef\xbb\xbf"+content)
				require.NoError(t, decompressDatasetFile(context.Background(), file, int64(len(content))))
				require.Equal(t, int64(len(content)), file.DownloadSize)
				input := filepath.Join(file.LocalRepoPath, file.LocalFileName)
				got, err := os.ReadFile(input)
				require.NoError(t, err)
				require.Equal(t, content, string(got))
				writer, err := parquet.NewS3Writer(context.Background(), &config.Config{})
				require.NoError(t, err)
				defer writer.(io.Closer).Close()
				method := "read_json_auto"
				if format == ".csv" {
					method = "read_csv_auto"
				}
				out := filepath.Join(file.LocalRepoPath, "export")
				require.NoError(t, writer.ConvertToParquet(context.Background(), method, []string{fmt.Sprintf("'%s'", input)}, 1, out))
				paths, err := filepath.Glob(filepath.Join(out, "*.parquet"))
				require.NoError(t, err)
				reader := parquet.NewParquetGoReader(&parquet.OSFileClient{}, otel.Tracer("test"), "")
				_, _, _, count, err := reader.RowsWithCount(context.Background(), paths, 0, 0)
				require.NoError(t, err)
				require.Equal(t, int64(2), count)
			})
		}
	}
}

func TestCompressedDatasetRejectsBadInput(t *testing.T) {
	for _, compression := range []string{".gz", ".zst"} {
		for _, kind := range []string{"limit", "truncated", "checksum", "canceled"} {
			t.Run(compression+kind, func(t *testing.T) {
				file := compressedFixture(t, ".jsonl", compression, strings.Repeat("{\"x\":1}\n", 100))
				path := filepath.Join(file.LocalRepoPath, file.LocalFileName)
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				limit := int64(4096)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				switch kind {
				case "limit":
					limit = 10
				case "truncated":
					data = data[:len(data)-3]
				case "checksum":
					data[len(data)-4] ^= 0xff
				case "canceled":
					cancel()
				}
				require.NoError(t, os.WriteFile(path, data, 0o600))
				require.Error(t, decompressDatasetFile(ctx, file, limit))
				entries, err := os.ReadDir(file.LocalRepoPath)
				require.NoError(t, err)
				require.Len(t, entries, 1, "failed decompression must not publish partial data")
				require.Equal(t, filepath.Base(path), entries[0].Name())
			})
		}
	}
}

func TestCompressedSourceCannotUsePartialDownload(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "cache"))
	require.NoError(t, err)
	defer f.Close()
	activity := &dataViewerActivityImpl{}
	err = activity.copyFileContent(f, io.NopCloser(strings.NewReader("compressed")), dvCom.FileObject{RepoFile: "train.jsonl.gz", Size: 100, DownloadSize: 99}, &dvCom.FileObject{}, ".jsonl.gz")
	require.ErrorContains(t, err, "compressed source exceeds")
}

func TestCompressedActivityBudgetAcrossSplits(t *testing.T) {
	for _, fits := range []bool{true, false} {
		t.Run(fmt.Sprint(fits), func(t *testing.T) {
			content := strings.Repeat("{\"x\":1}\n", 100)
			file := compressedFixture(t, ".jsonl", ".gz", content)
			data, err := os.ReadFile(filepath.Join(file.LocalRepoPath, file.LocalFileName))
			require.NoError(t, err)
			cfg := &config.Config{}
			cfg.DataViewer.CacheDir = t.TempDir()
			cfg.DataViewer.ConvertLimitSize = int64(2 * len(content))
			if !fits {
				cfg.DataViewer.ConvertLimitSize--
			}
			git := mockGit.NewMockGitServer(t)
			git.EXPECT().GetRepoFileReader(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, gitserver.GetRepoInfoByPathReq) (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
			}).Twice()
			act := &dataViewerActivityImpl{cfg: cfg, gitServer: git}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(act.DownloadSplitFiles)
			req := dvCom.DownloadFileReq{Req: types.UpdateViewerReq{Namespace: "test", Name: "compressed", Branch: "main", RepoType: types.DatasetRepo}}
			for _, split := range []string{"train", "test"} {
				req.Card.DatasetInfos = append(req.Card.DatasetInfos, dvCom.DatasetInfo{ConfigName: split, Splits: []dvCom.Split{{Name: split, Origins: []dvCom.FileObject{{RepoFile: split + ".jsonl.gz", Size: int64(len(data)), DownloadSize: int64(len(data))}}}}})
			}
			result, err := env.ExecuteActivity(act.DownloadSplitFiles, req)
			if !fits {
				require.ErrorContains(t, err, "conversion byte limit")
				return
			}
			require.NoError(t, err)
			var card dvCom.DownloadCard
			require.NoError(t, result.Get(&card))
			require.Len(t, card.Subsets, 2)
			for _, subset := range card.Subsets {
				f := subset.Splits[0].Files[0]
				require.Equal(t, "00000.jsonl", f.LocalFileName)
				require.Equal(t, int64(len(content)), f.DownloadSize)
			}
		})
	}
}
