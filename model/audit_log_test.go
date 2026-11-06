package model

import (
	"fmt"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			// GORM's PostgreSQL GetIndexes neither filters by schema nor keeps
			// key order, so read the indexes of this test's schema directly.
			type auditIndex struct {
				Name    string
				Columns string
				Unique  bool
			}
			readIndexes := func() map[string]auditIndex {
				var rows []auditIndex
				require.NoError(t, db.Raw(`
SELECT index_class.relname AS name,
       string_agg(attribute.attname, ',' ORDER BY index_column.ordinal_position) AS columns,
       index_meta.indisunique AS "unique"
FROM pg_catalog.pg_index AS index_meta
JOIN pg_catalog.pg_class AS index_class ON index_class.oid = index_meta.indexrelid
CROSS JOIN LATERAL unnest(index_meta.indkey) WITH ORDINALITY AS index_column(attnum, ordinal_position)
JOIN pg_catalog.pg_attribute AS attribute
  ON attribute.attrelid = index_meta.indrelid AND attribute.attnum = index_column.attnum
WHERE index_meta.indrelid = to_regclass('audit_logs')
GROUP BY index_class.relname, index_meta.indisunique
`).Scan(&rows).Error)
				byName := make(map[string]auditIndex, len(rows))
				for _, row := range rows {
					byName[row.Name] = row
				}
				return byName
			}
			originalIndexes := readIndexes()
			for range 2 {
				require.NoError(t, MigrateAuditLogs())
			}
			indexes := readIndexes()
			index, ok := indexes["idx_audit_time_event"]
			require.True(t, ok, "migration must create the audit ordering index")
			assert.Equal(t, "created_at,event_id", index.Columns)
			for name, original := range originalIndexes {
				assert.Equal(t, original, indexes[name], "existing index %s must remain unchanged", name)
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
