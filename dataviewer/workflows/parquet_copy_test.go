package workflows

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	mockGit "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/git/gitserver"
	mockdb "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/store/database"
	mockS3 "opencsg.com/csghub-server/_mocks/opencsg.com/csghub-server/builder/store/s3"
	"opencsg.com/csghub-server/builder/store/database"
	"opencsg.com/csghub-server/common/config"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/common/utils/common"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyParquetPromotesGitBlobToLFS(t *testing.T) {
	cfg := &config.Config{}
	cfg.DataViewer.CacheDir = t.TempDir()
	cfg.S3.Bucket = "test"
	gs := mockGit.NewMockGitServer(t)
	s3 := mockS3.NewMockClient(t)
	store := mockdb.NewMockLfsMetaObjectStore(t)
	a := &dataViewerActivityImpl{cfg: cfg, gitServer: gs, s3Client: s3, lfsMetaStore: store}
	payload := "parquet bytes transferred intact"
	sum := sha256.Sum256([]byte(payload))
	oid := hex.EncodeToString(sum[:])
	key := common.BuildLfsPath(42, oid, true)
	gs.EXPECT().GetRepoFileReader(mock.Anything, mock.Anything).Return(io.NopCloser(strings.NewReader(payload)), int64(len(payload)), nil)
	s3.EXPECT().PutObject(mock.Anything, "test", key, mock.Anything, int64(len(payload)), mock.Anything).RunAndReturn(func(ctx context.Context, bucket, key string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
		b, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, payload, string(b))
		return minio.UploadInfo{Size: size}, nil
	})
	store.EXPECT().UpdateOrCreate(mock.Anything, mock.MatchedBy(func(m database.LfsMetaObject) bool { return m.Oid == oid && m.RepositoryID == 42 && m.Existing })).Return(&database.LfsMetaObject{}, nil)
	gs.EXPECT().CreateRepoFile(mock.MatchedBy(func(r *types.CreateFileReq) bool {
		return r.FilePath == "default/train/00000.parquet" && len(r.Content) < 1024
	})).Return(nil)
	req := dvCom.CopyParquetReq{Req: types.UpdateViewerReq{RepoID: 42, Namespace: "root", Name: "test", RepoType: types.DatasetRepo, Branch: "main", Migrated: true}, NewBranch: "refs-convert-parquet"}
	copied, gotKey, err := a.copyParquetOrigin(context.Background(), req, dvCom.FileObject{RepoFile: "data.parquet"}, "default/train/00000.parquet", nil)
	require.NoError(t, err)
	require.Equal(t, key, gotKey)
	require.True(t, copied.Lfs)
	require.Equal(t, oid, copied.LfsSHA256)
	require.Empty(t, copied.ConvertPath)
	entries, err := os.ReadDir(cfg.DataViewer.CacheDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestDownloadLFSRejects404BeforeCopyingErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not parquet", 404) }))
	defer server.Close()
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	cfg := &config.Config{}
	cfg.S3.Bucket = "test"
	cfg.DataViewer.DownloadLfsFile = true
	s3 := mockS3.NewMockClient(t)
	oid := strings.Repeat("a", 64)
	key := common.BuildLfsPath(42, oid, true)
	s3.EXPECT().PresignedGetObject(mock.Anything, "test", key, mock.Anything, mock.Anything).Return(u, nil)
	a := &dataViewerActivityImpl{cfg: cfg, s3Client: s3}
	path := filepath.Join(t.TempDir(), "data.jsonl")
	err = a.downloadLfsFile(context.Background(), types.UpdateViewerReq{RepoID: 42, Migrated: true}, path, dvCom.FileObject{LfsSHA256: oid, LfsRelativePath: "misleading/legacy/path"}, &dvCom.FileObject{}, ".jsonl")
	require.ErrorContains(t, err, "HTTP 404")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

func TestWholeJSONCopyRemovesBOM(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "data.jsonl"))
	require.NoError(t, err)
	defer f.Close()
	raw := "\xef\xbb\xbf{\"id\":1}\n"
	target := dvCom.FileObject{Size: int64(len(raw))}
	a := &dataViewerActivityImpl{}
	err = a.copyFileContent(f, io.NopCloser(strings.NewReader(raw)), dvCom.FileObject{Size: int64(len(raw)), DownloadSize: int64(len(raw))}, &target, ".jsonl")
	require.NoError(t, err)
	got, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	require.Equal(t, "{\"id\":1}\n", string(got))
	require.Equal(t, int64(len(got)), target.DownloadSize)
}
