package workflows

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/types"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

func TestRepoFiles_appendFile(t *testing.T) {
	file := &types.File{
		Name: "test.jsonl",
		Size: 101,
	}

	fileClass := dvCom.RepoFilesClass{
		AllFiles:     make(map[string]*dvCom.RepoFile),
		ParquetFiles: make(map[string]*dvCom.RepoFile),
		JsonlFiles:   make(map[string]*dvCom.RepoFile),
		CsvFiles:     make(map[string]*dvCom.RepoFile),
	}

	appendFile(file, &fileClass, 100)

	require.Equal(t, 1, len(fileClass.AllFiles))
	require.Equal(t, 1, len(fileClass.JsonlFiles))
}

func TestScanExcludesMacOSSidecarsWithoutConsumingDatasetBudget(t *testing.T) {
	files := &dvCom.RepoFilesClass{
		AllFiles: make(map[string]*dvCom.RepoFile), ParquetFiles: make(map[string]*dvCom.RepoFile),
		JsonlFiles: make(map[string]*dvCom.RepoFile), CsvFiles: make(map[string]*dvCom.RepoFile),
		MediaFiles: make(map[string]*dvCom.RepoFile),
	}
	for _, name := range []string{"train/._data.jsonl", "._test.parquet", "__MACOSX/data.csv", "nested/__MACOSX/picture.png", ".DS_Store"} {
		appendFile(&types.File{Name: filepath.Base(name), Path: name, Size: 4096}, files, 100)
	}
	appendFile(&types.File{Name: "data.jsonl", Path: "train/data.jsonl", Size: 80}, files, 100)
	require.Len(t, files.AllFiles, 1)
	require.Contains(t, files.JsonlFiles, "train/data.jsonl")
	require.Equal(t, int64(80), files.TotalJsonSize)
	require.Zero(t, files.TotalParquetSize)
	require.Zero(t, files.TotalCsvSize)
	require.Zero(t, files.TotalMediaSize)
}

func TestIsValidMediaFile(t *testing.T) {
	valid := []string{
		"videos/00001.mp4", "data/clip.MP4", "img/cat.jpg", "img/cat.JPEG", "a.png",
		"voice.wav", "voice.flac", "movie.mkv", "movie.webm", "anim.gif",
	}
	for _, name := range valid {
		require.True(t, IsValidMediaFile(name), "expected %s to be a media file", name)
	}

	invalid := []string{
		"train.parquet", "data.jsonl", "data.csv", "README.md", "captions.txt",
		"mp4", "videos/", "archive.mp4.bak",
	}
	for _, name := range invalid {
		require.False(t, IsValidMediaFile(name), "expected %s to not be a media file", name)
	}
}

func TestAppendFile_MediaFiles(t *testing.T) {
	newFileClass := func() *dvCom.RepoFilesClass {
		return &dvCom.RepoFilesClass{
			AllFiles:     make(map[string]*dvCom.RepoFile),
			ParquetFiles: make(map[string]*dvCom.RepoFile),
			JsonlFiles:   make(map[string]*dvCom.RepoFile),
			CsvFiles:     make(map[string]*dvCom.RepoFile),
			MediaFiles:   make(map[string]*dvCom.RepoFile),
		}
	}

	t.Run("media files are classified into MediaFiles", func(t *testing.T) {
		fc := newFileClass()
		appendFile(&types.File{Name: "00001.mp4", Path: "videos/00001.mp4", Size: 100}, fc, 1000)
		appendFile(&types.File{Name: "0.jpg", Path: "0.jpg", Size: 50}, fc, 1000)
		appendFile(&types.File{Name: "captions.txt", Path: "captions.txt", Size: 10}, fc, 1000)

		require.Len(t, fc.MediaFiles, 2)
		require.Contains(t, fc.MediaFiles, "videos/00001.mp4")
		require.Contains(t, fc.MediaFiles, "0.jpg")
		require.Equal(t, int64(150), fc.TotalMediaSize)
		require.Empty(t, fc.ParquetFiles)
		require.Empty(t, fc.JsonlFiles)
		require.Empty(t, fc.CsvFiles)
		require.Len(t, fc.AllFiles, 2)
	})

	t.Run("media files stop being collected over the limit size", func(t *testing.T) {
		fc := newFileClass()
		appendFile(&types.File{Name: "a.mp4", Path: "a.mp4", Size: 800}, fc, 1000)
		appendFile(&types.File{Name: "b.mp4", Path: "b.mp4", Size: 500}, fc, 1000)
		require.Equal(t, int64(1000), fc.TotalMediaSize)
		require.Equal(t, int64(200), fc.MediaFiles["b.mp4"].DownloadSize)
		appendFile(&types.File{Name: "c.mp4", Path: "c.mp4", Size: 10}, fc, 1000)
		require.NotContains(t, fc.MediaFiles, "c.mp4")
	})
}

func TestWriteMediaIndexFile(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "00000.jsonl")
	origins := []dvCom.FileObject{
		{RepoFile: "videos/00001.mp4"},
		{RepoFile: "videos/with,comma.mp4"},
		{RepoFile: `videos/with"quote.mp4`},
	}
	size, err := writeMediaIndexFile(indexPath, origins)
	require.NoError(t, err)

	content, err := os.ReadFile(indexPath)
	require.NoError(t, err)
	require.Equal(t, "{\"file\":\"videos/00001.mp4\"}\n{\"file\":\"videos/with,comma.mp4\"}\n{\"file\":\"videos/with\\\"quote.mp4\"}\n", string(content))
	require.Equal(t, int64(len(content)), size)
}
