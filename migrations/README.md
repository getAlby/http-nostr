# Creating a new migration

1. Create a new file based on the current date and time (see existing migration format)
2. Copy the following code and update MY_ID_HERE and MY_COMMENT_HERE and DO_SOMETHING_HERE
3. Add the ID to the list of migrations in migrate.go

```go
package migrations

import (
  "github.com/go-gormigrate/gormigrate/v2"
  "gorm.io/gorm"
)

// MY_COMMENT_HERE
var _MY_ID_HERE = &gormigrate.Migration {
  ID: "MY_ID_HERE",
  Migrate: func(tx *gorm.DB) error {
    return DO_SOMETHING_HERE.Error;
  },
  Rollback: func(tx *gorm.DB) error {
    return nil;
  },
}
```
