package workflows

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	dvCom "opencsg.com/csghub-server/dataviewer/common"
)

// Recognize single compressed tables, not archives or arbitrary gzip files.
func datasetFileFormat(name string) (format, compression string) {
	name = strings.ToLower(name)
	ext := filepath.Ext(name)
	if ext == ".gz" || ext == ".zst" {
		compression = ext
		name = strings.TrimSuffix(name, ext)
	}
	format = filepath.Ext(name)
	if format != FileExtName.Json && format != FileExtName.Jsonl && format != FileExtName.Csv {
		return "", ""
	}
	return format, compression
}

func openDatasetDecoder(source io.Reader, compression string) (io.Reader, func(), error) {
	if compression == ".gz" {
		r, err := gzip.NewReader(source)
		if err != nil {
			return nil, nil, err
		}
		return r, func() { _ = r.Close() }, nil
	}
	r, err := zstd.NewReader(source, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(128<<20))
	if err != nil {
		return nil, nil, err
	}
	return r, r.Close, nil
}

type datasetContextReader struct {
	ctx context.Context
	io.Reader
}

func (r datasetContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

// The budget applies to expanded bytes across the whole download activity.
// Publish only after EOF (including checksum validation); never count a prefix.
func decompressDatasetFile(ctx context.Context, file *dvCom.FileObject, remaining int64) error {
	format, compression := datasetFileFormat(file.RepoFile)
	if compression == "" {
		return nil
	}
	if remaining <= 0 {
		return fmt.Errorf("decompressed dataset exceeds conversion byte limit")
	}
	dir := filepath.Join(file.LocalRepoPath, file.SubsetName, file.SplitName)
	sourcePath := filepath.Join(dir, file.LocalFileName)
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	reader, closeReader, err := openDatasetDecoder(datasetContextReader{ctx, source}, compression)
	if err != nil {
		return fmt.Errorf("open compressed dataset: %w", err)
	}
	defer closeReader()
	name := strings.TrimSuffix(file.LocalFileName, filepath.Ext(file.LocalFileName))
	if filepath.Ext(name) != format {
		return fmt.Errorf("compressed cache file has unexpected format")
	}
	copied, err := writeExpandedDataset(ctx, reader, filepath.Join(dir, name), remaining)
	if err != nil {
		return err
	}
	if err := os.Remove(sourcePath); err != nil {
		return err
	}
	file.LocalFileName = name
	file.DownloadSize = copied
	return nil
}

// Keep incomplete or checksum-invalid output outside the converter input list.
func writeExpandedDataset(ctx context.Context, reader io.Reader, destination string, remaining int64) (int64, error) {
	reader = withoutUTF8BOM(reader)
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".dataset-decompress-")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	copied, err := io.Copy(tmp, io.LimitReader(datasetContextReader{ctx, reader}, remaining))
	if err != nil {
		return 0, fmt.Errorf("decompress dataset: %w", err)
	}
	var extra [1]byte
	n, err := io.ReadFull(datasetContextReader{ctx, reader}, extra[:])
	if n > 0 {
		return 0, fmt.Errorf("decompressed dataset exceeds conversion byte limit")
	}
	if err != io.EOF {
		return 0, fmt.Errorf("validate compressed dataset: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), destination); err != nil {
		return 0, err
	}
	return copied, nil
}
