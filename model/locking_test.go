package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/utils/tests"
)

// lockForUpdate must always emit FOR UPDATE.
func TestLockForUpdateEmitsRowLock(t *testing.T) {
	dummyDB, err := gorm.Open(tests.DummyDialector{}, &gorm.Config{DryRun: true})
	require.NoError(t, err)
	var rows []Redemption
	sql := lockForUpdate(dummyDB).Where("id = ?", 1).Find(&rows).Statement.SQL.String()
	assert.Contains(t, sql, "FOR UPDATE")
}
