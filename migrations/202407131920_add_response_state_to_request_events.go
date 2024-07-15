package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

// Add response_received column to request_events table
var _202407131920_add_response_state_to_request_events = &gormigrate.Migration{
	ID: "202407131920_add_response_state_to_request_events",
	Migrate: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE request_events ADD COLUMN response_received BOOLEAN DEFAULT FALSE").Error; err != nil {
			return err
		}

		// Update response_received to TRUE if there is a corresponding row in response_events
		if err := tx.Exec(`
			UPDATE request_events
			SET response_received = TRUE
			WHERE id IN (SELECT request_id FROM response_events WHERE request_id IS NOT NULL)
		`).Error; err != nil {
			return err
		}

		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		if err := tx.Exec("ALTER TABLE request_events DROP COLUMN response_received").Error; err != nil {
			return err
		}
		return nil
	},
}
