package migrations

import (
	_ "embed"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/gorm"
)

//go:embed 202402161653_initial_migration.sql
var initialMigration string

// Initial migration
var _202402161653_initial_migration = &gormigrate.Migration{
	ID: "202402161653_initial_migration",
	Migrate: func(tx *gorm.DB) error {
		return tx.Exec(initialMigration).Error
	},
	Rollback: func(tx *gorm.DB) error {
		return nil
	},
}
