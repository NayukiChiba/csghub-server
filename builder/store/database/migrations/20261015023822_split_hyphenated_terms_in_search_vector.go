package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"opencsg.com/csghub-server/common/config"
)

// Index the hyphen/underscore/slash-split form of description and readme in
// addition to the raw text: the opencsgchinese tokenizer keeps
// "text-classification" as one lexeme, so a query for "classification" (or the
// 分类 synonym group) could never prefix-match it. HF dataset cards declare
// tasks almost exclusively as hyphenated task_categories/task_ids tokens, so
// this made the richest task metadata invisible to full-text search. The raw
// form is kept so hyphenated query terms still match the original lexeme.
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

		if err := updateRepoSearchVectorTriggerSplitHyphen(ctx, db, cfg.Database.SearchConfiguration, "[-_/]"); err != nil {
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

		// restore the readme-aware trigger without the hyphen-split form (the
		// shape 20261013061635_add_readme_to_repo_search_vector.go left)
		if err := updateRepoSearchVectorTriggerWithReadme(ctx, db, cfg.Database.SearchConfiguration, "[-_/]"); err != nil {
			return err
		}
		return initRepositorySearchVectorInBatches(ctx, db)
	})
}

// updateRepoSearchVectorTriggerSplitHyphen recreates
// update_repo_search_vector() like updateRepoSearchVectorTriggerWithReadme,
// plus a second pass over description and readme with hyphens/underscores/
// slashes replaced by spaces so constituent words are indexed as their own
// lexemes.
func updateRepoSearchVectorTriggerSplitHyphen(ctx context.Context, db *bun.DB, searchConfiguration, separatorClass string) error {
	_, err := db.ExecContext(ctx, `
		CREATE OR REPLACE FUNCTION update_repo_search_vector() RETURNS trigger AS $$
		BEGIN
			NEW.search_vector :=
				setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.path, '`+separatorClass+`', ' ', 'g'), '')), 'A')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.hf_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.ms_path, '[-/]', ' ', 'g'), '')), 'B')
				|| setweight(to_tsvector('`+searchConfiguration+`',COALESCE(regexp_replace(NEW.nickname, '`+separatorClass+`', ' ', 'g'), '')), 'C')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(NEW.description, '')), 'D')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(regexp_replace(NEW.description, '`+separatorClass+`', ' ', 'g'), '')), 'D')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(left(NEW.readme, 100000), '')), 'D')
				|| setweight(to_tsvector('`+searchConfiguration+`', COALESCE(regexp_replace(left(NEW.readme, 100000), '`+separatorClass+`', ' ', 'g'), '')), 'D');
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
