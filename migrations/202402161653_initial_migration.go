package migrations

import (
	_ "embed"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

//go:embed initial_migration_postgres.sql
var initialMigration string

// Initial migration
var _202402161653_initial_migration = &gormigrate.Migration {
	ID: "202402161653_initial_migration",
	Migrate: func(tx *gorm.DB) error {
		// only execute migration if subscriptions table doesn't exist
		err := tx.Exec("SELECT * FROM subscriptions").Error;
		if err != nil {
			return tx.Exec(initialMigration).Error
		}
		return nil
	},
	Rollback: func(tx *gorm.DB) error {
		return nil;
	},
}
