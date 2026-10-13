package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"opencsg.com/csghub-server/common/config"
)

// Add repositories.content_lang: the dataset content language detected from
// the description's script (zh/en). Full-text search cannot express "english
// datasets" — English text never says "english" — so language filtering gets
// a structured column instead of matching the word in name/description.
// Maintained by a trigger on description writes; backfilled in batches.
func init() {
	Migrations.MustRegister(func(ctx context.Context, db *bun.DB) error {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Database.Driver != "pg" {
			return nil
		}

		if _, err := db.ExecContext(ctx, `
			ALTER TABLE repositories ADD COLUMN IF NOT EXISTS content_lang text NOT NULL DEFAULT '';

			CREATE OR REPLACE FUNCTION update_repo_content_lang() RETURNS trigger AS $$
			DECLARE
				cjk   int;
				latin int;
			BEGIN
				-- a real Chinese sentence far exceeds 8 han characters; an
				-- English description with a few han characters stays 'en'
				cjk := char_length(COALESCE(NEW.description, '')) - char_length(regexp_replace(COALESCE(NEW.description, ''), '[一-鿿]', '', 'g'));
				latin := char_length(regexp_replace(COALESCE(NEW.description, ''), '[^A-Za-z]', '', 'g'));
				NEW.content_lang := CASE
					WHEN cjk >= 8 THEN 'zh'
					WHEN latin >= 16 THEN 'en'
					ELSE ''
				END;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;

			CREATE OR REPLACE TRIGGER repositories_content_lang_update
			BEFORE INSERT OR UPDATE OF description
			ON repositories
			FOR EACH ROW
			EXECUTE FUNCTION update_repo_content_lang();
		`); err != nil {
			return fmt.Errorf("failed to create content_lang column and trigger: %w", err)
		}

		// backfill in batches: touching description fires the trigger
		const batchSize = 1000
		offset := 0
		for {
			var batchIDs []int64
			if err := db.NewSelect().Table("repositories").Column("id").Order("id").
				Limit(batchSize).Offset(offset).Scan(ctx, &batchIDs); err != nil {
				return fmt.Errorf("failed to get batch IDs (offset %d): %w", offset, err)
			}
			if len(batchIDs) == 0 {
				break
			}
			if _, err := db.NewUpdate().Table("repositories").Set("description = description").
				Where("id IN (?)", bun.In(batchIDs)).Exec(ctx); err != nil {
				return fmt.Errorf("failed to backfill content_lang (offset %d): %w", offset, err)
			}
			offset += batchSize
		}
		return nil
	}, func(ctx context.Context, db *bun.DB) error {
		cfg, err := config.LoadConfig()
		if err != nil {
			return err
		}
		if cfg.Database.Driver != "pg" {
			return nil
		}
		if _, err := db.ExecContext(ctx, `
			DROP TRIGGER IF EXISTS repositories_content_lang_update ON repositories;
			DROP FUNCTION IF EXISTS update_repo_content_lang();
			ALTER TABLE repositories DROP COLUMN IF EXISTS content_lang;
		`); err != nil {
			return fmt.Errorf("failed to drop content_lang: %w", err)
		}
		return nil
	})
}
