package parquet_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	parquetgo "github.com/parquet-go/parquet-go"
	"github.com/spf13/cast"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"opencsg.com/csghub-server/builder/parquet"
)

type mediaValue struct {
	Bytes []byte `parquet:"bytes"`
	Path  string `parquet:"path"`
}

type multimodalRow struct {
	ID     int64      `parquet:"id"`
	Image  mediaValue `parquet:"image"`
	Audio  mediaValue `parquet:"audio"`
	Video  []byte     `parquet:"video"`
	Labels []string   `parquet:"labels,list"`
}

func initReader() *parquet.ParquetGoReader {
	return parquet.NewParquetGoReader(&parquet.OSFileClient{}, otel.Tracer("test"), "")
}

// test data: 10 parquet files, each file contains 20 rows of data, row id start from 0
func TestGoReader_All(t *testing.T) {
	cases := []struct {
		limit         int64
		offset        int64
		expectedRange string
	}{
		{10, 0, "0-9"},
		{10, 10, "10-19"},
		{10, 18, "18-27"},
		{30, 18, "18-47"},
		{60, 185, "185-199"},
		{100, 75, "75-174"},
	}

	paths := []string{}
	for i := 0; i < 10; i++ {
		paths = append(paths, fmt.Sprintf("test_data/%d.parquet", i))
	}

	reader := initReader()
	for _, c := range cases {
		t.Run(fmt.Sprintf("%+v", c), func(t *testing.T) {
			columns, columnTypes, data, total, err := reader.RowsWithCount(
				context.TODO(),
				paths,
				c.limit,
				c.offset,
			)
			require.NoError(t, err)
			require.Equal(t, []string{"Id", "Name"}, columns)
			require.Equal(t, []string{"INT64", "INT64"}, columnTypes)
			require.Equal(t, int64(200), total)

			rg := strings.Split(c.expectedRange, "-")
			start := cast.ToInt64(rg[0])
			end := cast.ToInt64(rg[1])
			current := start
			for _, row := range data {
				id := cast.ToInt64(row[0])
				name := cast.ToInt64(row[1])
				require.Equal(t, current, id)
				require.Equal(t, current, name)
				current += 1
			}
			require.Equal(t, end+1, current)
		})
	}

}

func TestGoReader_UUID(t *testing.T) {
	paths := []string{}
	paths = append(paths, "test_data/uuid.parquet")

	reader := initReader()
	t.Run("test-uuid-column", func(t *testing.T) {
		columns, columnTypes, data, total, err := reader.RowsWithCount(
			context.TODO(),
			paths,
			2,
			0,
		)
		require.NoError(t, err)
		require.Equal(t, []string{"id", "image"}, columns)
		require.Equal(t, []string{"FIXED_LEN_BYTE_ARRAY", "BYTE_ARRAY"}, columnTypes)
		require.Equal(t, int64(2), total)

		realData := [][]string{
			{
				"0ad08b5c-35ba-4304-9eaa-1afb0554e459",
				"image.png",
			},
			{
				"1e4fe944-78ef-4fd5-abf5-cb4a4a20ac86",
				"21000.png",
			},
		}

		for idx, row := range data {
			id := cast.ToString(row[0])
			name := cast.ToString(row[1])
			require.Equal(t, realData[idx][0], id)
			require.Equal(t, realData[idx][1], name)
		}

	})

}

func TestGoReader_MultimodalNestedColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multimodal.parquet")
	imageBytes := []byte("sixteen-byte-img")
	audioBytes := []byte("audio-content")
	videoBytes := []byte("video-content")
	err := parquetgo.WriteFile(path, []multimodalRow{{
		ID: 1,
		Image: mediaValue{
			Bytes: imageBytes,
			Path:  "images/example.png",
		},
		Audio: mediaValue{
			Bytes: audioBytes,
			Path:  "audio/example.wav",
		},
		Video:  videoBytes,
		Labels: []string{"cat", "speech"},
	}})
	require.NoError(t, err)

	columns, columnTypes, data, total, err := initReader().RowsWithCount(
		context.Background(),
		[]string{path},
		10,
		0,
	)
	require.NoError(t, err)
	require.Equal(t, []string{"id", "image", "audio", "video", "labels"}, columns)
	require.Equal(t, []string{"INT64", "STRUCT", "STRUCT", "BYTE_ARRAY", "LIST"}, columnTypes)
	require.Equal(t, int64(1), total)
	require.Len(t, data, 1)

	image, ok := data[0][1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, base64.StdEncoding.EncodeToString(imageBytes), image["bytes"])
	require.Equal(t, "images/example.png", image["path"])

	audio, ok := data[0][2].(map[string]any)
	require.True(t, ok)
	require.Equal(t, base64.StdEncoding.EncodeToString(audioBytes), audio["bytes"])
	require.Equal(t, "audio/example.wav", audio["path"])
	require.Equal(t, base64.StdEncoding.EncodeToString(videoBytes), data[0][3])
	require.Equal(t, []any{"cat", "speech"}, data[0][4])
}
