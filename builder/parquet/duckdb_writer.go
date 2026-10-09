package parquet

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
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

func (r *duckdbWriter) ConvertToParquet(ctx context.Context, method string, objNames []string, thread int, path string) error {
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
	fileStr := ""
	if len(objNames) > 1 {
		fileStr = "[" + strings.Join(objNames, ",") + "]"
	} else {
		fileStr = objNames[0]
	}
	exportCMD := fmt.Sprintf(`
	SET preserve_insertion_order=false;
	SET threads = %d; 
	COPY (SELECT * FROM %s(%s)) TO '%s' (
		FORMAT 'PARQUET', 
		CODEC 'Snappy', 
		PER_THREAD_OUTPUT TRUE, 
		FILENAME_PATTERN '{i}' 
	);
	`, thread, method, fileStr, path)
	slog.Debug("export parquet sql", slog.String("sql", exportCMD))
	_, err := r.db.ExecContext(ctx, exportCMD)
	if err != nil {
		return fmt.Errorf("failed to export parquet: %w", err)
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
