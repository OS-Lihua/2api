package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestAuditLogSortIndexDatabase verifies audit ordering and index migrations
// on a PostgreSQL log database.
func TestAuditLogSortIndexDatabase(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
			// UseTestDatabases restores the log database and its type.
			db := UseTestDatabases(t)
			LOG_DB = db
			common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
			require.NoError(t, MigrateAuditLogs())
			if upgrade {
				require.NoError(t, db.Migrator().DropIndex(&AuditLog{}, "idx_audit_time_event"))
			}
			entries := []AuditLog{
				{EventId: "event-older", CreatedAt: 100, UserId: 1, ActorRole: common.RoleCommonUser},
				{EventId: "event-a", CreatedAt: 200, UserId: 1, ActorRole: common.RoleCommonUser},
				{EventId: "event-z", CreatedAt: 200, UserId: 1, ActorRole: common.RoleCommonUser},
			}
			require.NoError(t, db.Create(&entries).Error)
			originalIndexes, err := db.Migrator().GetIndexes(&AuditLog{})
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, MigrateAuditLogs())
			}
			indexes, err := db.Migrator().GetIndexes(&AuditLog{})
			require.NoError(t, err)
			byName := make(map[string]gorm.Index, len(indexes))
			for _, index := range indexes {
				byName[index.Name()] = index
			}
			index, ok := byName["idx_audit_time_event"]
			require.True(t, ok, "migration must create the audit ordering index")
			var columns []string
			// GORM's PostgreSQL GetIndexes does not preserve index key order.
			require.NoError(t, db.Raw(`
SELECT attribute.attname
FROM pg_catalog.pg_index AS index_meta
CROSS JOIN LATERAL unnest(index_meta.indkey) WITH ORDINALITY AS index_column(attnum, ordinal_position)
JOIN pg_catalog.pg_attribute AS attribute
  ON attribute.attrelid = index_meta.indrelid AND attribute.attnum = index_column.attnum
WHERE index_meta.indexrelid = to_regclass(?)
ORDER BY index_column.ordinal_position
`, index.Name()).Scan(&columns).Error)
			assert.Equal(t, []string{"created_at", "event_id"}, columns)
			for _, original := range originalIndexes {
				preserved, ok := byName[original.Name()]
				require.True(t, ok, "existing index %s must remain", original.Name())
				assert.Equal(t, original.Columns(), preserved.Columns(), original.Name())
				originalUnique, originalKnown := original.Unique()
				preservedUnique, preservedKnown := preserved.Unique()
				assert.Equal(t, originalKnown, preservedKnown, original.Name())
				assert.Equal(t, originalUnique, preservedUnique, original.Name())
			}
			var stored []AuditLog
			require.NoError(t, db.Order("id").Find(&stored).Error)
			assert.Equal(t, entries, stored, "migration must preserve existing audit records")
			logs, total, err := GetAuditLogs(AuditLogFilter{}, 0, 2, common.RoleRootUser)
			require.NoError(t, err)
			assert.EqualValues(t, 3, total)
			require.Len(t, logs, 2)
			assert.Equal(t, []string{"event-z", "event-a"}, []string{logs[0].EventId, logs[1].EventId})
			logs, total, err = GetAuditLogs(AuditLogFilter{}, 2, 2, common.RoleRootUser)
			require.NoError(t, err)
			assert.EqualValues(t, 3, total)
			require.Len(t, logs, 1)
			assert.Equal(t, "event-older", logs[0].EventId)
			assert.Error(t, db.Create(&AuditLog{EventId: "event-a"}).Error, "event IDs must remain unique")
		})
	}
}
