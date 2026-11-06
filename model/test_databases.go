package model

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/testdb"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// UseTestDatabases opens isolated databases for t, assigns them to DB and
// LOG_DB, sets the database types and column names, and restores the
// previous state when t ends. It returns the primary database.
//
// With TEST_POSTGRES_DSN set, the primary database is a fresh PostgreSQL
// schema. With TEST_CLICKHOUSE_LOG_DSN also set, the log database is a fresh
// ClickHouse database with the log tables already created; otherwise the log
// database is the primary database. Without TEST_POSTGRES_DSN, tests fall back
// to an in-memory SQLite database until SQLite support is removed (#54).
func UseTestDatabases(t testdb.T) *gorm.DB {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		initCol()
	})

	db, mainType := openTestPrimary(t)
	logDB, logType := db, mainType
	if dsn := testdb.ClickHouseLogDSN(t); dsn != "" {
		var err error
		logDB, logType, err = OpenDSN(testdb.ClickHouseLogEnv, dsn, true)
		if err != nil {
			t.Fatalf("testdb: open ClickHouse log database: %v", err)
		}
		t.Cleanup(func() { closeTestDB(logDB) })
	}

	DB, LOG_DB = db, logDB
	common.SetDatabaseTypes(mainType, logType)
	initCol()
	if logType == common.DatabaseTypeClickHouse {
		if err := migrateLOGDB(); err != nil {
			t.Fatalf("testdb: migrate ClickHouse log database: %v", err)
		}
	}
	return db
}

func openTestPrimary(t testdb.T) (*gorm.DB, common.DatabaseType) {
	t.Helper()
	if dsn := testdb.PostgresDSN(t); dsn != "" {
		db, dbType, err := OpenDSN(testdb.PostgresEnv, dsn, false)
		if err != nil {
			t.Fatalf("testdb: open PostgreSQL: %v", err)
		}
		t.Cleanup(func() { closeTestDB(db) })
		return db, dbType
	}
	name := "t_" + common.GetRandomString(12)
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("testdb: open SQLite: %v", err)
	}
	t.Cleanup(func() { closeTestDB(db) })
	return db, common.DatabaseTypeSQLite
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
