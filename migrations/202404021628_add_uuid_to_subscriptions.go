package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add UUID column to subscriptions table
var _202404021628_add_uuid_to_subscriptions = &gormigrate.Migration{
	ID: "202404021628_add_uuid_to_subscriptions",
	Migrate: func(tx *gorm.DB) error {
		return tx.Exec("ALTER TABLE subscriptions ADD COLUMN uuid UUID DEFAULT gen_random_uuid()").Error
	},
	Rollback: func(tx *gorm.DB) error {
		return tx.Exec("ALTER TABLE subscriptions DROP COLUMN IF EXISTS uuid").Error
	},
}