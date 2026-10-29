package parquet

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
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

func TestCSVConversionPreservesMixedTimestampRepresentations(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.csv")
	b := filepath.Join(root, "b.csv")
	require.NoError(t, os.WriteFile(a, []byte("timestamp,id\n2026-03-18 01:02:03,001\n"), 0600))
	require.NoError(t, os.WriteFile(b, []byte("timestamp,id\n1773820897000,002\n"), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{fmt.Sprintf("'%s'", a), fmt.Sprintf("'%s'", b)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?) WHERE (id='001' AND timestamp='2026-03-18 01:02:03') OR (id='002' AND timestamp='1773820897000')", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 2, count)
}

func TestJSONConversionPreservesMixedTimestampStrings(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	input := filepath.Join(root, "input.json")
	require.NoError(t, os.WriteFile(input, []byte(`[{"events":{"created":"2023-10-22T15:00:00Z","updated":"2023-10-22 15:00:00"}}]`), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?) WHERE events.created='2023-10-22T15:00:00Z' AND events.updated='2023-10-22 15:00:00'", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 1, count)
}

func TestJSONConvertedNestedKeysCanBeCountedAndPreviewed(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	input := filepath.Join(root, "input.json")
	require.NoError(t, os.WriteFile(input, []byte(`[{"answers":{"0":"zero","a-b":"dash","a b":"space"},"messages":[{"0":"nested"}]}]`), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	paths, err := filepath.Glob(filepath.Join(output, "*.parquet"))
	require.NoError(t, err)
	reader := NewParquetGoReader(&OSFileClient{}, otel.Tracer("test"), "")
	for _, limit := range []int64{0, 10} {
		columns, types, data, total, err := reader.RowsWithCount(context.Background(), paths, limit, 0)
		require.NoError(t, err)
		require.Equal(t, int64(1), total)
		require.Equal(t, []string{"answers", "messages"}, columns)
		require.Equal(t, []string{"STRUCT", "STRUCT"}, types)
		if limit > 0 {
			require.Len(t, data, 1)
			require.Equal(t, map[string]any{"0": "zero", "a-b": "dash", "a b": "space"}, data[0][0])
			require.Equal(t, map[string]any{"list": []any{map[string]any{"element": map[string]any{"0": "nested"}}}}, data[0][1])
		}
	}
}

func TestJSONConversionIncludesTypesBeyondInitialSample(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	input := filepath.Join(root, "input.jsonl")
	content := strings.Repeat("{\"value\":42}\n", 20480) + "{\"value\":\"147.247.152.19\"}\n"
	require.NoError(t, os.WriteFile(input, []byte(content), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?)", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 20481, count)
	var text string
	require.NoError(t, w.db.QueryRow("SELECT CAST(value AS VARCHAR) FROM read_parquet(?) WHERE CAST(value AS VARCHAR) LIKE '%147.247.152.19%'", filepath.Join(output, "*.parquet")).Scan(&text))
	require.Contains(t, text, "147.247.152.19")
}

func TestCSVConversionUsesEachFilesHeaderAndDialect(t *testing.T) {
	w := newRecoveryWriter(t)
	root := t.TempDir()
	a := filepath.Join(root, "a.csv")
	b := filepath.Join(root, "b.csv")
	require.NoError(t, os.WriteFile(a, []byte("source,repo,skill,classification,pattern,severity\na,b,c,d,e,f\n"), 0600))
	require.NoError(t, os.WriteFile(b, []byte("source;repo;skill;classification;url\nx;y;z;safe;example\n"), 0600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{fmt.Sprintf("'%s'", a), fmt.Sprintf("'%s'", b)}, 1, output))
	paths, err := filepath.Glob(filepath.Join(output, "*.parquet"))
	require.NoError(t, err)
	reader := NewParquetGoReader(&OSFileClient{}, otel.Tracer("test"), "")
	columns, _, data, total, err := reader.RowsWithCount(context.Background(), paths, 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(2), total)
	require.Len(t, data, 2)
	require.ElementsMatch(t, []string{"source", "repo", "skill", "classification", "pattern", "severity", "url"}, columns)
}

func TestCSVConversionWithoutHeaderPreservesFirstRecord(t *testing.T) {
	w := newRecoveryWriter(t)
	w.cfg.DataViewer.CSVHeader = "false"
	root := t.TempDir()
	input := filepath.Join(root, "input.csv")
	output := filepath.Join(root, "output")
	require.NoError(t, os.WriteFile(input, []byte("alice,001\nbob,002\n"), 0600))
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_csv_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	var count int
	require.NoError(t, w.db.QueryRow("SELECT count(*) FROM read_parquet(?)", filepath.Join(output, "*.parquet")).Scan(&count))
	require.Equal(t, 2, count)
}
