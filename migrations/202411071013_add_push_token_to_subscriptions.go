package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add push_token column to subscriptions table
var _202411071013_add_push_token_to_subscriptions = &gormigrate.Migration{
	ID: "202411071013_add_push_token_to_subscriptions",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN push_token TEXT").Error; err != nil {
			return err
		}
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS subscriptions_push_token ON subscriptions (push_token)").Error; err != nil {
			return err
		}
		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS subscriptions_push_token").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN push_token").Error; err != nil {
			return err
		}
		return nil
	},
}
