package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"opencsg.com/csghub-server/common/config"
)

// Split underscores (in addition to dashes and slashes) when building
// repositories.search_vector: snake_case names like "librispeech_asr_dummy"
// were indexed as a single lexeme, so prefix queries like 'asr:*' never
// matched. Rebuilds the trigger function and re-indexes existing rows in
// batches.
func init() {
	Migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Database.Driver != "pg" {
			return nil
		}

		var searchVectorExists bool
		if err := db.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = 'repositories' AND column_name = 'search_vector')
		`).Scan(&searchVectorExists); err != nil {
			return fmt.Errorf("failed to check whether the search_vector column exists: %w", err)
		}
		if !searchVectorExists {
			return nil
		}

		if err := updateRepoSearchVectorTrigger(ctx, db, cfg.Database.SearchConfiguration, "[-_/]"); err != nil {
			return err
		}
		return initRepositorySearchVectorInBatches(ctx, db)
	}, func(ctx context.Context, db *bun.DB) error {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Database.Driver != "pg" {
			return nil
		}

		var searchVectorExists bool
		if err := db.QueryRow(`
			SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name = 'repositories' AND column_name = 'search_vector')
		`).Scan(&searchVectorExists); err != nil {
			return fmt.Errorf("failed to check whether the search_vector column exists: %w", err)
		}
		if !searchVectorExists {
			return nil
		}

		if err := updateRepoSearchVectorTrigger(ctx, db, cfg.Database.SearchConfiguration, "[-/]"); err != nil {
			return err
		}
		return initRepositorySearchVectorInBatches(ctx, db)
	})
}

// updateRepoSearchVectorTrigger recreates update_repo_search_vector() with the
// given separator character class used to split path-like columns.
func updateRepoSearchVectorTrigger(ctx context.Context, db *bun.DB, searchConfiguration, separatorClass string) error {
	_, err := db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION update_repo_search_vector() RETURNS trigger AS $$
		BEGIN
			NEW.search_vector :=
				setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.path, '`+separatorClass+`', ' ', 'g'), '')), 'A')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.hf_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.ms_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.nickname, '`+separatorClass+`', ' ', 'g'), '')), 'C')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(NEW.description, '')), 'D');
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE OR REPLACE TRIGGER repositories_search_vector_update
		BEFORE INSERT OR UPDATE OF path, nickname, description, hf_path, ms_path
		ON repositories
		FOR EACH ROW
		EXECUTE FUNCTION update_repo_search_vector();
	`)
	if err != nil {
		return fmt.Errorf("failed to update search vector configuration: %w", err)
	}
	return nil
}
