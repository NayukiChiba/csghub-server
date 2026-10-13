package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"opencsg.com/csghub-server/common/config"
)

// Include repositories.readme in search_vector (weight D, truncated to 100k
// chars) so dataset README content — task annotations like
// `task_ids: multiple-choice` in HF card front-matter, usage descriptions —
// becomes full-text searchable. The readme column already exists and is
// populated by sync/callback flows, so only the trigger and a reindex are
// needed. Rebuilds the trigger function and re-indexes existing rows in
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

		if err := updateRepoSearchVectorTriggerWithReadme(ctx, db, cfg.Database.SearchConfiguration, "[-_/]"); err != nil {
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

		// restore the trigger without the readme part (same shape as
		// 20261009101034_update_tsvector_split_underscore.go left it)
		if err := updateRepoSearchVectorTrigger(ctx, db, cfg.Database.SearchConfiguration, "[-_/]"); err != nil {
			return err
		}
		return initRepositorySearchVectorInBatches(ctx, db)
	})
}

// updateRepoSearchVectorTriggerWithReadme recreates update_repo_search_vector()
// like updateRepoSearchVectorTrigger, plus the readme column at weight D.
// left(readme, 100000) caps the input so oversized READMEs cannot blow the
// tsvector size limit; the trigger fires on readme updates too.
func updateRepoSearchVectorTriggerWithReadme(ctx context.Context, db *bun.DB, searchConfiguration, separatorClass string) error {
	_, err := db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION update_repo_search_vector() RETURNS trigger AS $$
		BEGIN
			NEW.search_vector :=
				setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.path, '`+separatorClass+`', ' ', 'g'), '')), 'A')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.hf_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.ms_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.nickname, '`+separatorClass+`', ' ', 'g'), '')), 'C')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(NEW.description, '')), 'D')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(left(NEW.readme, 100000), '')), 'D');
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		CREATE OR REPLACE TRIGGER repositories_search_vector_update
		BEFORE INSERT OR UPDATE OF path, nickname, description, hf_path, ms_path, readme
		ON repositories
		FOR EACH ROW
		EXECUTE FUNCTION update_repo_search_vector();
	`)
	if err != nil {
		return fmt.Errorf("failed to update search vector configuration: %w", err)
	}
	return nil
}
