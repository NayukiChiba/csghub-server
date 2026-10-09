package parquet_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"opencsg.com/csghub-server/builder/parquet"
	"opencsg.com/csghub-server/common/config"
)

func TestDuckDBWriter_LocalConversionDoesNotRequireRemoteExtensions(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		ext     string
		content string
	}{
		{
			name:   "csv",
			method: "read_csv_auto",
			ext:    ".csv",
			content: `id,text
1,hello
2,world
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			input := filepath.Join(root, "input"+tt.ext)
			require.NoError(t, os.WriteFile(input, []byte(tt.content), 0o600))
			output := filepath.Join(root, "output")
			require.NoError(t, os.Mkdir(output, 0o700))

			writer, err := parquet.NewS3Writer(context.Background(), &config.Config{})
			require.NoError(t, err)
			require.NoError(t, writer.ConvertToParquet(
				context.Background(), tt.method, []string{fmt.Sprintf("'%s'", input)}, 1, output+"/",
			))

			entries, err := os.ReadDir(output)
			require.NoError(t, err)
			require.NotEmpty(t, entries)
			paths := make([]string, 0, len(entries))
			for _, entry := range entries {
				paths = append(paths, filepath.Join(output, entry.Name()))
			}
			reader := parquet.NewParquetGoReader(&parquet.OSFileClient{}, otel.Tracer("test"), "")
			_, _, _, total, err := reader.RowsWithCount(context.Background(), paths, 0, 0)
			require.NoError(t, err)
			require.Equal(t, int64(2), total)
		})
	}
}
