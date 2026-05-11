package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add connection_error_count, last_connection_error and closed_reason columns
// to subscriptions so we can auto-disable subscriptions whose relay is unreachable.
var _202605111200_add_connection_error_tracking_to_subscriptions = &gormigrate.Migration{
	ID: "202605111200_add_connection_error_tracking_to_subscriptions",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN connection_error_count INTEGER NOT NULL DEFAULT 0").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN last_connection_error TIMESTAMP NULL").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN closed_reason TEXT NOT NULL DEFAULT ''").Error; err != nil {
			return err
		}
		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN closed_reason").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN last_connection_error").Error; err != nil {
			return err
		}
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN connection_error_count").Error; err != nil {
			return err
		}
		return nil
	},
}
