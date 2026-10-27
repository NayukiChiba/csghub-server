package parquet

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/require"
	"opencsg.com/csghub-server/common/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newRecoveryWriter(t *testing.T) *duckdbWriter {
	t.Helper()
	db, err := sql.Open("duckdb", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return &duckdbWriter{db: db, cfg: &config.Config{}}
}

func TestConversionRetryReplacesShardsWithoutDuplicatingRows(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	input := filepath.Join(root, "input.csv")
	output := filepath.Join(root, "output")
	require.NoError(t, os.WriteFile(input, []byte("id,text\n1,hello\n2,world\n"), 0600))
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	// Simulate a leftover shard from a larger/partial previous attempt.
	files, err := filepath.Glob(filepath.Join(output, "*.parquet"))
	require.NoError(t, err)
	require.NotEmpty(t, files)
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(output, "stale.parquet"), data, 0600))
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?)", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 2, count)
	_, err = os.Stat(filepath.Join(output, "stale.parquet"))
	require.True(t, os.IsNotExist(err))
}

func TestConversionFailurePreservesPreviousOutput(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	output := filepath.Join(root, "output")
	require.NoError(t, os.Mkdir(output, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(output, "old.parquet"), []byte("original"), 0600))
	require.Error(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{"'/does-not-exist.csv'"}, 1, output))
	data, err := os.ReadFile(filepath.Join(output, "old.parquet"))
	require.NoError(t, err)
	require.Equal(t, "original", string(data))
}

func TestConversionAcceptsLargeJSONAndDifferentColumns(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.jsonl")
	b := filepath.Join(root, "b.jsonl")
	require.NoError(t, os.WriteFile(a, []byte(`{"text":"`+strings.Repeat("x", 17<<20)+`"}`+"\n"), 0600))
	require.NoError(t, os.WriteFile(b, []byte("{\"other\":42}\n"), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", a), fmt.Sprintf("'%s'", b)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?)", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 2, count)
}
