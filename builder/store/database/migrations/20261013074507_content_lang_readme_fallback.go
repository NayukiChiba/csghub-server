package migrations

import (
	"context"
	"fmt"

	"github.com/uptrace/bun"
	"opencsg.com/csghub-server/common/config"
)

// Let update_repo_content_lang() fall back to the README when the description
// alone cannot decide the language: mirrored repos often carry no description
// at all, their dataset card lives in the git README. The trigger now also
// fires on readme writes (git callback / sync populate readme long after
// insert), and the backfill is re-run so existing rows pick the fallback up.
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
			CREATE OR REPLACE FUNCTION update_repo_content_lang() RETURNS trigger AS $$
			DECLARE
				cjk   int;
				latin int;
				src   text;
			BEGIN
				-- prefer the description; a real Chinese sentence far exceeds
				-- 8 han characters, an English description with a few han
				-- characters stays 'en'
				src := COALESCE(NEW.description, '');
				cjk := char_length(src) - char_length(regexp_replace(src, '[一-鿿]', '', 'g'));
				latin := char_length(regexp_replace(src, '[^A-Za-z]', '', 'g'));
				IF cjk < 8 AND latin < 16 THEN
					src := left(COALESCE(NEW.readme, ''), 20000);
					cjk := char_length(src) - char_length(regexp_replace(src, '[一-鿿]', '', 'g'));
					latin := char_length(regexp_replace(src, '[^A-Za-z]', '', 'g'));
				END IF;
				NEW.content_lang := CASE
					WHEN cjk >= 8 THEN 'zh'
					WHEN latin >= 16 THEN 'en'
					ELSE ''
				END;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;

			CREATE OR REPLACE TRIGGER repositories_content_lang_update
			BEFORE INSERT OR UPDATE OF description, readme
			ON repositories
			FOR EACH ROW
			EXECUTE FUNCTION update_repo_content_lang();
		`); err != nil {
			return fmt.Errorf("failed to add readme fallback to content_lang trigger: %w", err)
		}

		// re-run the backfill so rows whose language only the README reveals
		// get reclassified
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
				return fmt.Errorf("failed to re-backfill content_lang (offset %d): %w", offset, err)
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
		// restore the description-only variant from
		// 20261013071850_add_content_lang_to_repositories
		if _, err := db.ExecContext(ctx, `
			CREATE OR REPLACE FUNCTION update_repo_content_lang() RETURNS trigger AS $$
			DECLARE
				cjk   int;
				latin int;
			BEGIN
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
			return fmt.Errorf("failed to restore description-only content_lang trigger: %w", err)
		}
		return nil
	})
}
