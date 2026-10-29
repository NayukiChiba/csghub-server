package parquet

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	parquetgo "github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/snappy"
)

const jsonRowGroupBytes = 8 << 20

type jsonRecord struct {
	Record string `parquet:"record"`
}

// Unlike vectorized JSON readers, this path never batches thousands of large
// records in memory. Flush by input bytes as well as by row count.
func writeJSONRecords(ctx context.Context, paths []string, output string, maxRecordBytes int64) (err error) {
	f, err := os.Create(filepath.Join(output, "0.parquet"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	w := parquetgo.NewGenericWriter[jsonRecord](f,
		parquetgo.Compression(&snappy.Codec{}), parquetgo.MaxRowsPerRowGroup(2048),
		parquetgo.PageBufferSize(64<<10), parquetgo.SkipPageBounds("record"))
	defer func() { err = errors.Join(err, w.Close()) }()
	buffered := 0
	appendRecord := func(raw json.RawMessage) error {
		if _, err := w.Write([]jsonRecord{{Record: string(raw)}}); err != nil {
			return err
		}
		buffered += len(raw)
		if buffered >= jsonRowGroupBytes {
			buffered = 0
			return w.Flush()
		}
		return nil
	}
	for _, path := range paths {
		if err := readJSONRecords(ctx, path, maxRecordBytes, appendRecord); err != nil {
			return fmt.Errorf("read JSON records from %s: %w", path, err)
		}
	}
	return nil
}

// The decoder can read ahead. Bound its underlying reader relative to the last
// consumed offset, allowing a small separator/whitespace margin. Check the exact
// record length too, including when the record was already buffered.
type jsonRecordReader struct {
	ctx    context.Context
	source io.Reader
	read   int64
	end    int64
}

func (r *jsonRecordReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.read >= r.end {
		return 0, fmt.Errorf("JSON record exceeds configured byte limit")
	}
	if int64(len(p)) > r.end-r.read {
		p = p[:r.end-r.read]
	}
	n, err := r.source.Read(p)
	r.read += int64(n)
	return n, err
}

func readJSONRecords(ctx context.Context, path string, limit int64, emit func(json.RawMessage) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	source := bufio.NewReader(f)
	if prefix, _ := source.Peek(3); string(prefix) == "\xef\xbb\xbf" {
		_, _ = source.Discard(3)
	}
	r := &jsonRecordReader{ctx: ctx, source: source, end: limit + 64<<10}
	buffered := bufio.NewReader(r)
	for {
		b, err := buffered.Peek(1)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if !strings.ContainsRune(" \t\n\r", rune(b[0])) {
			break
		}
		_, _ = buffered.ReadByte()
	}
	first, _ := buffered.Peek(1)
	array := first[0] == '['
	// Account for bytes consumed while detecting the root, and bufio read-ahead.
	start := r.read - int64(buffered.Buffered())
	d := json.NewDecoder(buffered)
	if array {
		if _, err := d.Token(); err != nil {
			return err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.end = start + d.InputOffset() + limit + 64<<10
		if array && !d.More() {
			return finishJSONArray(d)
		}
		var raw json.RawMessage
		if err := d.Decode(&raw); err != nil {
			if !array && errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if int64(len(raw)) > limit {
			return fmt.Errorf("JSON record exceeds configured byte limit: %d > %d", len(raw), limit)
		}
		if err := emit(raw); err != nil {
			return err
		}
	}
}

func finishJSONArray(d *json.Decoder) error {
	end, err := d.Token()
	if err != nil {
		return err
	}
	if end != json.Delim(']') {
		return fmt.Errorf("JSON array is not closed")
	}
	var extra json.RawMessage
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected data after JSON array")
	}
	return nil
}

func localJSONPaths(objects []string) ([]string, bool) {
	paths := make([]string, 0, len(objects))
	for _, object := range objects {
		if len(object) < 2 || object[0] != '\'' || object[len(object)-1] != '\'' {
			return nil, false
		}
		path := strings.ReplaceAll(object[1:len(object)-1], "''", "'")
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, false
		}
		paths = append(paths, path)
	}
	return paths, true
}
