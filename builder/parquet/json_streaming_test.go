package parquet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	parquetgo "github.com/parquet-go/parquet-go"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
)

func TestLargeJSONStreamsCompleteRecordsWithoutTypeInference(t *testing.T) {
	w := newRecoveryWriter(t)
	w.cfg.DataViewer.JSONInferenceMaxBytes = 1
	w.cfg.DataViewer.ConversionMemoryLimit = 256 << 20
	w.cfg.DataViewer.MaxJSONSize = 1 << 20
	root := t.TempDir()
	a, b := filepath.Join(root, "array.json"), filepath.Join(root, "rows.jsonl")
	require.NoError(t, os.WriteFile(a, []byte(`[{"value":1,"nested":{"1":{"x":"yes"}}},{"value":{"different":true}}]`), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("{\"late_field\":\"unchanged\"}\n"), 0o600))
	output := filepath.Join(root, "output")
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", a), fmt.Sprintf("'%s'", b)}, 1, output))
	paths, err := filepath.Glob(filepath.Join(output, "*.parquet"))
	require.NoError(t, err)
	reader := NewParquetGoReader(&OSFileClient{}, otel.Tracer("test"), "")
	columns, _, rows, count, err := reader.RowsWithCount(context.Background(), paths, 10, 0)
	require.NoError(t, err)
	require.Equal(t, int64(3), count)
	require.Equal(t, []string{"record"}, columns)
	var actual []string
	for _, row := range rows {
		actual = append(actual, row[0].(string))
	}
	require.ElementsMatch(t, []string{`{"value":1,"nested":{"1":{"x":"yes"}}}`, `{"value":{"different":true}}`, `{"late_field":"unchanged"}`}, actual)
}

func TestJSONStreamingRejectsMalformedRecords(t *testing.T) {
	for _, content := range []string{"{\"ok\":1}\nnot json\n", `{"x":"\uZZZZ"}`} {
		w := newRecoveryWriter(t)
		w.cfg.DataViewer.JSONInferenceMaxBytes = 1
		root := t.TempDir()
		input, output := filepath.Join(root, "input.jsonl"), filepath.Join(root, "output")
		require.NoError(t, os.WriteFile(input, []byte(content), 0o600))
		require.Error(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
		_, err := os.Stat(output)
		require.True(t, os.IsNotExist(err))
	}
}

func TestJSONInferenceMemoryFailureFallsBackToCompleteRecords(t *testing.T) {
	w := newRecoveryWriter(t)
	w.cfg.DataViewer.ConversionMemoryLimit = 1 << 20
	root := t.TempDir()
	input, output := filepath.Join(root, "input.jsonl"), filepath.Join(root, "output")
	require.NoError(t, os.WriteFile(input, []byte("{\"id\":1}\n"), 0o600))
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	rows, err := parquetgo.ReadFile[jsonRecord](filepath.Join(output, "0.parquet"))
	require.NoError(t, err)
	require.Equal(t, []jsonRecord{{Record: `{"id":1}`}}, rows)
}

func TestRawJSONRowGroupsAreBoundedByBytes(t *testing.T) {
	w := newRecoveryWriter(t)
	w.cfg.DataViewer.JSONInferenceMaxBytes = 1
	root := t.TempDir()
	input, output := filepath.Join(root, "input.jsonl"), filepath.Join(root, "output")
	record := `{"value":"` + strings.Repeat("x", 64<<10) + `"}`
	require.NoError(t, os.WriteFile(input, []byte(strings.Repeat(record+"\n", 320)), 0600))
	require.NoError(t, w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output))
	f, err := os.Open(filepath.Join(output, "0.parquet"))
	require.NoError(t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(t, err)
	p, err := parquetgo.OpenFile(f, info.Size())
	require.NoError(t, err)
	require.Equal(t, int64(320), p.NumRows())
	require.Greater(t, len(p.RowGroups()), 1)
	for _, group := range p.RowGroups() {
		require.LessOrEqual(t, group.NumRows(), int64(128))
	}
	rows, err := parquetgo.ReadFile[jsonRecord](f.Name())
	require.NoError(t, err)
	require.Len(t, rows, 320)
	for _, row := range rows {
		require.Equal(t, record, row.Record)
	}
}

func TestRawJSONRecordLimitAndCancellationPreservePreviousOutput(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		w := newRecoveryWriter(t)
		w.cfg.DataViewer.JSONInferenceMaxBytes = 1
		w.cfg.DataViewer.MaxJSONSize = 1024
		root := t.TempDir()
		input, output := filepath.Join(root, "input.jsonl"), filepath.Join(root, "output")
		require.NoError(t, os.Mkdir(output, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(output, "old.parquet"), []byte("unchanged"), 0600))
		require.NoError(t, os.WriteFile(input, []byte(`{"text":"`+strings.Repeat("x", 1<<20)+`"}`), 0600))
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		}
		err := w.ConvertToParquet(ctx, "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output)
		cancel()
		if cancelled {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			require.ErrorContains(t, err, "byte limit")
		}
		old, err := os.ReadFile(filepath.Join(output, "old.parquet"))
		require.NoError(t, err)
		require.Equal(t, "unchanged", string(old))
		partials, err := filepath.Glob(filepath.Join(root, ".parquet-export-*"))
		require.NoError(t, err)
		require.Empty(t, partials)
	}
}

func TestRawJSONValidatesArrayBoundaryAndExactRecordLimit(t *testing.T) {
	for _, content := range []string{"[] {}", "[{}", "[{},]", "[{}] rubbish"} {
		path := filepath.Join(t.TempDir(), "bad.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		require.Error(t, readJSONRecords(context.Background(), path, 100, func(_ json.RawMessage) error { return nil }))
	}
	record := `{"text":"` + strings.Repeat("a", 1024) + `"}`
	for _, content := range []string{"\xef\xbb\xbf [" + strings.TrimSuffix(strings.Repeat(record+",", 100), ",") + "]", strings.Repeat(record+"\n", 100)} {
		path := filepath.Join(t.TempDir(), "valid.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
		count := 0
		require.NoError(t, readJSONRecords(context.Background(), path, int64(len(record)), func(raw json.RawMessage) error {
			require.Equal(t, record, string(raw))
			count++
			return nil
		}))
		require.Equal(t, 100, count)
	}
}

func TestJSONInferenceBudgetIncludesAllFiles(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a'quoted.json"), filepath.Join(root, "b.json")
	require.NoError(t, os.WriteFile(a, []byte("{}"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("{}"), 0o600))
	objects := []string{fmt.Sprintf("'%s/a''quoted.json'", root), fmt.Sprintf("'%s'", b)}
	require.True(t, jsonInputsFitInferenceBudget(objects, 4))
	require.False(t, jsonInputsFitInferenceBudget(objects, 3))
	require.False(t, jsonInputsFitInferenceBudget([]string{"'s3://bucket/data.jsonl'"}, 100))
	require.False(t, jsonInputsFitInferenceBudget([]string{"'/missing.json'"}, 100))
}
