package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add response_received_at column to request_events table
var _202407171220_add_response_received_at_to_request_events = &gormigrate.Migration{
	ID: "202407171220_add_response_received_at_to_request_events",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE request_events ADD COLUMN response_received_at TIMESTAMP NULL").Error; err != nil {
			return err
		}

		// Update response_received_at if there is a corresponding row in response_events
		if err := tx.Exec(`
			UPDATE request_events SET response_received_at = created_at
		`).Error; err != nil {
			return err
		}

		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE request_events DROP COLUMN response_received_at").Error; err != nil {
			return err
		}
		return nil
	},
}
