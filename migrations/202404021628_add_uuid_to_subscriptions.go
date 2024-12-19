package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add UUID column to subscriptions table
var _202404021628_add_uuid_to_subscriptions = &gormigrate.Migration{
	ID: "202404021628_add_uuid_to_subscriptions",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN uuid UUID DEFAULT gen_random_uuid()").Error; err != nil {
			return err
		}
		return tx.Exec("CREATE INDEX IF NOT EXISTS subscriptions_uuid ON subscriptions (uuid)").Error
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS subscriptions_uuid").Error; err != nil {
			return err
		}
		return tx.Exec("ALTER TABLE subscriptions DROP COLUMN IF EXISTS uuid").Error
	},
}
