package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/testdb"

	"gorm.io/gorm"
)

// UseTestDatabases opens isolated databases for t, assigns them to DB and
// LOG_DB, sets the log database type, and restores the previous state when t
// ends. It returns the primary database.
//
// The primary database is a fresh schema on the PostgreSQL server named by
// TEST_POSTGRES_DSN; the test fails when it is unset. With
// TEST_CLICKHOUSE_LOG_DSN also set, the log database is a fresh ClickHouse
// database with the log tables already created; otherwise the log database is
// the primary database.
func UseTestDatabases(t testdb.T) *gorm.DB {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	previousLog := common.LogDatabaseType()
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetLogDatabaseType(previousLog)
	})

	dsn := testdb.PostgresDSN(t)
	if dsn == "" {
		t.Fatalf("testdb: %s is not set; backend tests need a PostgreSQL server, for example %s=postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable", testdb.PostgresEnv, testdb.PostgresEnv)
	}
	db, logType, err := OpenDSN(testdb.PostgresEnv, dsn, false)
	if err != nil {
		t.Fatalf("testdb: open PostgreSQL: %v", err)
	}
	t.Cleanup(func() { closeTestDB(db) })

	logDB := db
	if dsn := testdb.ClickHouseLogDSN(t); dsn != "" {
		logDB, logType, err = OpenDSN(testdb.ClickHouseLogEnv, dsn, true)
		if err != nil {
			t.Fatalf("testdb: open ClickHouse log database: %v", err)
		}
		t.Cleanup(func() { closeTestDB(logDB) })
	}

	DB, LOG_DB = db, logDB
	common.SetLogDatabaseType(logType)
	if logType == common.DatabaseTypeClickHouse {
		if err := migrateLOGDB(); err != nil {
			t.Fatalf("testdb: migrate ClickHouse log database: %v", err)
		}
	}
	return db
}

func closeTestDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// ClearTestLogTable deletes every row from a log-database table such as
// "logs" or "audit_logs". ClickHouse needs TRUNCATE because DELETE runs as an
// asynchronous mutation.
func ClearTestLogTable(table string) {
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		LOG_DB.Exec("TRUNCATE TABLE " + table)
		return
	}
	LOG_DB.Exec("DELETE FROM " + table)
}
