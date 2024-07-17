package migrations

import (
	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

func Migrate(db *gorm.DB) error {

	m := gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{
		_202402161653_initial_migration,
		_202404021628_add_uuid_to_subscriptions,
		_202404031539_add_indexes,
		_202407171220_add_response_received_at_to_request_events,
	})

	return m.Migrate()
}