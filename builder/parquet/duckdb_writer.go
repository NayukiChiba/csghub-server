package parquet

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/XSAM/otelsql"
	_ "github.com/marcboeker/go-duckdb"
	"opencsg.com/csghub-server/common/config"
)

type Writer interface {
	ConvertToParquet(ctx context.Context, method string, objNames []string, thread int, path string) error
}

type duckdbWriter struct {
	db      *sql.DB
	cfg     *config.Config
	s3Mu    sync.Mutex
	s3Ready bool
}

func NewS3Writer(ctx context.Context, cfg *config.Config) (Writer, error) {
	db, err := otelsql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("failed to connect to duckdb, cause:%w", err)
	}
	if err := otelsql.RegisterDBStatsMetrics(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to report db stats metrics, cause:%w", err)
	}
	return &duckdbWriter{db: db, cfg: cfg}, nil
}

func (r *duckdbWriter) setupS3(ctx context.Context) error {
	r.s3Mu.Lock()
	defer r.s3Mu.Unlock()
	if r.s3Ready {
		return nil
	}

	urlStyle := "vhost"
	if r.cfg.S3.BucketLookup == "path" {
		urlStyle = "path"
	}
	s3SetupSQL := fmt.Sprintf(`
	INSTALL httpfs;
	LOAD httpfs;
	SET s3_region = '%s';
	SET s3_endpoint = '%s';
	SET s3_access_key_id = '%s';
	SET s3_secret_access_key = '%s';
	SET s3_use_ssl = %t;
	SET s3_url_style = '%s';
	`, r.cfg.S3.Region, r.cfg.S3.Endpoint, r.cfg.S3.AccessKeyID, r.cfg.S3.AccessKeySecret, r.cfg.S3.EnableSSL, urlStyle)
	if _, err := r.db.ExecContext(ctx, s3SetupSQL); err != nil {
		return fmt.Errorf("failed to setup s3 for duckdb, cause:%w", err)
	}
	r.s3Ready = true
	slog.Debug("setup duckdb succeeded")
	return nil
}

// Close releases per-activity DuckDB resources.
func (r *duckdbWriter) Close() error { return r.db.Close() }

func (r *duckdbWriter) ConvertToParquet(ctx context.Context, method string, objNames []string, thread int, path string) error {
	if len(objNames) == 0 || thread < 1 {
		return fmt.Errorf("conversion requires input files and positive thread count")
	}
	if method != "read_json_auto" && method != "read_csv_auto" {
		return fmt.Errorf("unsupported conversion reader %q", method)
	}
	output, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(output); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("export path must be a directory")
	}
	// Convert into a fresh directory on every attempt. Failed retries must neither
	// reuse partial files nor publish a mix of old and new shards.
	staging, err := os.MkdirTemp(filepath.Dir(output), ".parquet-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)

	if slicesContainS3Path(objNames) {
		if err := r.setupS3(ctx); err != nil {
			return err
		}
	}
	if method == "read_json_auto" {
		if _, err := r.db.ExecContext(ctx, "INSTALL json; LOAD json;"); err != nil {
			return fmt.Errorf("failed to setup json for duckdb, cause:%w", err)
		}
	}
	if method == "read_csv_auto" {
		if err := r.exportCSVFiles(ctx, objNames, thread, staging); err != nil {
			return err
		}
	} else {
		limit := r.cfg.DataViewer.MaxJSONSize
		if limit <= 0 {
			limit = 128 << 20
		}
		query := r.jsonExportQuery(objNames, limit)
		if err := r.exportQuery(ctx, query, thread, staging); err != nil {
			return err
		}
	}
	return publishParquetExport(staging, output)
}

// Full JSON type inference can retain many times the input size in native
// memory. Large or remote sources are streamed as complete JSON records instead.
func (r *duckdbWriter) jsonExportQuery(objects []string, objectLimit int64) string {
	budget := r.cfg.DataViewer.JSONInferenceMaxBytes
	if budget <= 0 {
		budget = 64 << 20
	}
	if !jsonInputsFitInferenceBudget(objects, budget) {
		return fmt.Sprintf("SELECT json AS record FROM read_json_objects([%s], format='auto', maximum_object_size=%d)", strings.Join(objects, ","), objectLimit)
	}
	options := fmt.Sprintf(", union_by_name=true, maximum_object_size=%d, sample_size=-1, timestampformat='%s', dateformat='%s'", objectLimit, "%Y-%m-%dT%H:%M:%S.%fZ", "%Y-%m-%d")
	return fmt.Sprintf("SELECT * FROM read_json_auto([%s]%s)", strings.Join(objects, ","), options)
}

func jsonInputsFitInferenceBudget(objects []string, budget int64) bool {
	for _, object := range objects {
		if len(object) < 2 || object[0] != '\'' || object[len(object)-1] != '\'' {
			return false
		}
		path := strings.ReplaceAll(object[1:len(object)-1], "''", "'")
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > budget {
			return false
		}
		budget -= info.Size()
	}
	return true
}

// CSV files may use different column counts or dialects. DuckDB's multi-file
// reader can reuse the first file's dialect even with union_by_name enabled.
func (r *duckdbWriter) exportCSVFiles(ctx context.Context, objects []string, threads int, staging string) error {
	header := r.cfg.DataViewer.CSVHeader
	if header == "" {
		header = "true"
	}
	if header != "true" && header != "false" && header != "auto" {
		return fmt.Errorf("invalid CSV header mode %q: expected true, false, or auto", header)
	}
	options := ", all_varchar=true"
	if header != "auto" {
		options += ", header=" + header
	}
	sequence := 0
	for i, object := range objects {
		dir := filepath.Join(staging, fmt.Sprintf("csv-%d", i))
		query := fmt.Sprintf("SELECT * FROM read_csv_auto(%s%s)", object, options)
		if err := r.exportQuery(ctx, query, threads, dir); err != nil {
			return err
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, file := range files {
			// UploadParquetFiles uses a numeric stem to assign repository paths.
			name := fmt.Sprintf("%d.parquet", sequence)
			if err := os.Rename(filepath.Join(dir, file.Name()), filepath.Join(staging, name)); err != nil {
				return err
			}
			sequence++
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}

func (r *duckdbWriter) exportQuery(ctx context.Context, query string, threads int, path string) error {
	memoryLimit := r.cfg.DataViewer.ConversionMemoryLimit
	if memoryLimit <= 0 {
		memoryLimit = 2 << 30
	}
	command := fmt.Sprintf(`
	SET memory_limit = '%dB';
	SET preserve_insertion_order=false;
	SET threads = %d;
	COPY (%s) TO '%s' (
		FORMAT 'PARQUET', CODEC 'Snappy', PER_THREAD_OUTPUT TRUE, FILENAME_PATTERN '{i}', ROW_GROUP_SIZE 2048
	);`, memoryLimit, threads, query, strings.ReplaceAll(path+"/", "'", "''"))
	if _, err := r.db.ExecContext(ctx, command); err != nil {
		return fmt.Errorf("failed to export parquet: %w", err)
	}
	return nil
}

// Only replace converter-owned parquet shards. Refuse foreign entries instead
// of recursively deleting a caller-supplied directory.
func publishParquetExport(staging, output string) error {
	if err := os.MkdirAll(output, 0o700); err != nil {
		return err
	}
	old, err := os.ReadDir(output)
	if err != nil {
		return err
	}
	for _, f := range old {
		if !f.Type().IsRegular() || filepath.Ext(f.Name()) != ".parquet" {
			return fmt.Errorf("export directory contains non-parquet entry %q", f.Name())
		}
	}
	for _, f := range old {
		if err := os.Remove(filepath.Join(output, f.Name())); err != nil {
			return err
		}
	}
	fresh, err := os.ReadDir(staging)
	if err != nil {
		return err
	}
	for _, f := range fresh {
		if err := os.Rename(filepath.Join(staging, f.Name()), filepath.Join(output, f.Name())); err != nil {
			return err
		}
	}
	return nil
}

func slicesContainS3Path(paths []string) bool {
	for _, path := range paths {
		if strings.Contains(path, "s3://") {
			return true
		}
	}
	return false
}
