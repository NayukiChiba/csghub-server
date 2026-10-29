package parquet

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
	var memory string
	require.NoError(t, w.db.QueryRow("SELECT current_setting('memory_limit')").Scan(&memory))
	require.NotEmpty(t, memory)
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

func TestJSONConversionReportsMemoryBudgetFailure(t *testing.T) {
	w := newRecoveryWriter(t)
	w.cfg.DataViewer.JSONInferenceMaxBytes = 1
	w.cfg.DataViewer.ConversionMemoryLimit = 1 << 20
	root := t.TempDir()
	input, output := filepath.Join(root, "input.jsonl"), filepath.Join(root, "output")
	require.NoError(t, os.WriteFile(input, []byte("{\"id\":1}\n"), 0o600))
	err := w.ConvertToParquet(context.Background(), "read_json_auto", []string{fmt.Sprintf("'%s'", input)}, 1, output)
	require.ErrorContains(t, err, "Out of Memory")
	_, err = os.Stat(output)
	require.True(t, os.IsNotExist(err))
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
