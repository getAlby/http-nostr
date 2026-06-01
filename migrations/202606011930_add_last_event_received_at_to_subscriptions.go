package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

var _202606011930_add_last_event_received_at_to_subscriptions = &gormigrate.Migration{
	ID: "202606011930_add_last_event_received_at_to_subscriptions",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions ADD COLUMN last_event_received_at TIMESTAMP NULL").Error; err != nil {
			return err
		}
		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE subscriptions DROP COLUMN last_event_received_at").Error; err != nil {
			return err
		}
		return nil
	},
}
