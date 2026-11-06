package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Parallel tests share one server, so each Use call must see only its own rows.
func TestUseIsolatesDatabasesPerTest(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			db := UseTestDatabases(t)
			require.NoError(t, db.AutoMigrate(&Option{}))
			require.NoError(t, db.Create(&Option{Key: "isolation_probe", Value: name}).Error)

			var options []Option
			require.NoError(t, db.Find(&options).Error)
			require.Len(t, options, 1)
			assert.Equal(t, name, options[0].Value)
		})
	}
}

func TestUseCreatesLogTables(t *testing.T) {
	UseTestDatabases(t)
	require.NoError(t, MigrateLogDB())
	require.NoError(t, LOG_DB.Create(&Log{UserId: 1, Content: "probe"}).Error)

	var count int64
	require.NoError(t, LOG_DB.Model(&Log{}).Where("user_id = ?", 1).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}
