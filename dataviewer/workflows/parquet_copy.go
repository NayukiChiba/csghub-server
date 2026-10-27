package workflows

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"opencsg.com/csghub-server/builder/git/gitserver"
	"opencsg.com/csghub-server/common/types"
	"opencsg.com/csghub-server/common/utils/common"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

// copyParquetOrigin makes both Git blobs and existing LFS files available to
// the object-store reader. Ordinary Git blobs have no LFS object to count.
func (dva *dataViewerActivityImpl) copyParquetOrigin(ctx context.Context, req dvCom.CopyParquetReq, origin dvCom.FileObject, name string, files map[string]string) (dvCom.FileObject, string, error) {
	if !origin.Lfs {
		return dva.promoteParquetBlob(ctx, req, origin, name, files)
	}
	source := gitserver.GetRepoInfoByPathReq{Namespace: req.Req.Namespace, Name: req.Req.Name, RepoType: req.Req.RepoType, Ref: req.Req.Branch, Path: origin.RepoFile}
	f, err := dva.gitServer.GetRepoFileContents(ctx, source)
	if err != nil {
		return dvCom.FileObject{}, "", fmt.Errorf("read parquet pointer %s: %w", origin.RepoFile, err)
	}
	if _, exists := files[name]; exists {
		err = dva.gitServer.UpdateRepoFile(&types.UpdateFileReq{Username: GitDefaultUserName, Email: GitDefaultUserEmail, Message: "update parquet pointer", Namespace: req.Req.Namespace, Name: req.Req.Name, RepoType: req.Req.RepoType, Branch: req.NewBranch, FilePath: name, Content: f.Content})
	} else {
		err = dva.gitServer.CreateRepoFile(&types.CreateFileReq{Username: GitDefaultUserName, Email: GitDefaultUserEmail, Message: "create parquet pointer", Namespace: req.Req.Namespace, Name: req.Req.Name, RepoType: req.Req.RepoType, Branch: req.NewBranch, FilePath: name, Content: f.Content})
	}
	if err != nil {
		return dvCom.FileObject{}, "", fmt.Errorf("write parquet pointer %s: %w", name, err)
	}
	copied := dvCom.FileObject{RepoFile: name, Size: origin.Size, Lfs: true, LfsSHA256: origin.LfsSHA256, LfsRelativePath: origin.LfsRelativePath}
	key := common.SafeBuildLfsPath(req.Req.RepoID, origin.LfsSHA256, origin.LfsRelativePath, req.Req.Migrated)
	return copied, key, nil
}

func (dva *dataViewerActivityImpl) promoteParquetBlob(ctx context.Context, req dvCom.CopyParquetReq, origin dvCom.FileObject, name string, files map[string]string) (dvCom.FileObject, string, error) {
	source := gitserver.GetRepoInfoByPathReq{Namespace: req.Req.Namespace, Name: req.Req.Name, RepoType: req.Req.RepoType, Ref: req.Req.Branch, Path: origin.RepoFile}
	reader, _, err := dva.gitServer.GetRepoFileReader(ctx, source)
	if err != nil {
		return dvCom.FileObject{}, "", fmt.Errorf("read parquet blob %s: %w", origin.RepoFile, err)
	}
	defer reader.Close()
	if err := os.MkdirAll(dva.cfg.DataViewer.CacheDir, 0700); err != nil {
		return dvCom.FileObject{}, "", err
	}
	dir, err := os.MkdirTemp(dva.cfg.DataViewer.CacheDir, "parquet-blob-")
	if err != nil {
		return dvCom.FileObject{}, "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "data.parquet")
	f, err := os.Create(path)
	if err != nil {
		return dvCom.FileObject{}, "", err
	}
	size, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil {
		return dvCom.FileObject{}, "", copyErr
	}
	if closeErr != nil {
		return dvCom.FileObject{}, "", closeErr
	}
	copied := dvCom.FileObject{RepoFile: name, Size: size, Lfs: true, ConvertPath: path}
	oid, err := dva.uploadToRepo(ctx, req.Req, &copied, req.NewBranch, files)
	if err != nil {
		return dvCom.FileObject{}, "", err
	}
	copied.LfsSHA256 = oid
	copied.ConvertPath = ""
	return copied, common.BuildLfsPath(req.Req.RepoID, oid, req.Req.Migrated), nil
}
