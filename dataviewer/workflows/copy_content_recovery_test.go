package workflows

import (
	"errors"
	"github.com/stretchr/testify/require"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type brokenDatasetReader struct{}

func (brokenDatasetReader) Read(p []byte) (int, error) {
	return copy(p, "partial"), errors.New("source interrupted")
}

func TestCopyFilePreservesLongRecordsAndStripsBOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	record := `{"text":"` + strings.Repeat("a", 128<<10) + `"}` + "\n"
	n, err := CopyFileContext(f, io.NopCloser(strings.NewReader("\xef\xbb\xbf"+record+"{}\n")), 100)
	require.NoError(t, err)
	require.Equal(t, int64(len(record)), n)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, record, string(got))
}

func TestCopyFileReportsSourceErrors(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "broken"))
	require.NoError(t, err)
	defer f.Close()
	_, err = CopyFileContext(f, io.NopCloser(brokenDatasetReader{}), 1024)
	require.ErrorContains(t, err, "source interrupted")
}
