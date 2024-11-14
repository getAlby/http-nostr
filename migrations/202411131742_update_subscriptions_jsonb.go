package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Update subscriptions table to use JSONB columns for ids, kinds, authors, and tags
var _202411131742_update_subscriptions_jsonb = &gormigrate.Migration{
	ID: "202411131742_update_subscriptions_jsonb",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN ids_json jsonb").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN kinds_json jsonb").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN authors_json jsonb").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN tags_json jsonb").Error; err != nil {
			return err
		}

		if err := tx.Exec(`
			UPDATE subscriptions 
			SET ids_json = CASE 
				WHEN ids_string = '' THEN '[]'::jsonb 
				ELSE ids_string::jsonb 
			END 
			WHERE ids_string IS NOT NULL
		`).Error; err != nil {
			return err
		}
		
		if err := tx.Exec(`
			UPDATE subscriptions 
			SET kinds_json = CASE 
				WHEN kinds_string = '' THEN '[]'::jsonb 
				ELSE kinds_string::jsonb 
			END 
			WHERE kinds_string IS NOT NULL
		`).Error; err != nil {
			return err
		}
		
		if err := tx.Exec(`
			UPDATE subscriptions 
			SET authors_json = CASE 
				WHEN authors_string = '' THEN '[]'::jsonb 
				ELSE authors_string::jsonb 
			END 
			WHERE authors_string IS NOT NULL
		`).Error; err != nil {
			return err
		}

		if err := tx.Exec(`
			UPDATE subscriptions 
			SET tags_json = CASE 
				WHEN tags_string = '' THEN '{}'::jsonb 
				ELSE tags_string::jsonb 
			END 
			WHERE tags_string IS NOT NULL
		`).Error; err != nil {
			return err
		}

		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN ids_string").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN kinds_string").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN authors_string").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN tags_string").Error; err != nil {
			return err
		}

		if err := tx.Exec("CREATE INDEX IF NOT EXISTS subscriptions_open ON subscriptions (open)").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_subscriptions_authors_json ON subscriptions USING gin (authors_json)").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_subscriptions_tags_json ON subscriptions USING gin (tags_json)").Error; err != nil {
			return err
		}

		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS idx_subscriptions_authors_json").Error; err != nil {
			return err
		}
		if err := tx.Exec("DROP INDEX IF EXISTS idx_subscriptions_tags_json").Error; err != nil {
			return err
		}

		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN ids_string TEXT").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN kinds_string TEXT").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN authors_string TEXT").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN tags_string TEXT").Error; err != nil {
			return err
		}

		if err := tx.Exec("UPDATE subscriptions SET ids_string = ids_json::text WHERE ids IS NOT NULL").Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE subscriptions SET kinds_string = kinds_json::text WHERE kinds IS NOT NULL").Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE subscriptions SET authors_string = authors_json::text WHERE authors IS NOT NULL").Error; err != nil {
			return err
		}
		if err := tx.Exec("UPDATE subscriptions SET tags_string = tags_json::text WHERE tags IS NOT NULL").Error; err != nil {
			return err
		}

		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN ids_json").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN kinds_json").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN authors_json").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN tags_json").Error; err != nil {
			return err
		}

		return nil
	},
}
