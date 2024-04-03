package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add More Indexes to DB
var _202404031539_add_indexes = &gormigrate.Migration{
	ID: "202404031539_add_indexes",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE INDEX IF NOT EXISTS request_events_nostr_id ON request_events (nostr_id)").Error; err != nil {
			return err
		}

		if err := tx.Exec("CREATE INDEX IF NOT EXISTS subscriptions_open ON subscriptions (open)").Error; err != nil {
			return err
		}

		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("DROP INDEX IF EXISTS request_events_nostr_id").Error; err != nil {
			return err
		}
		if err := tx.Exec("DROP INDEX IF EXISTS subscriptions_open").Error; err != nil {
			return err
		}
		return nil
	},
}