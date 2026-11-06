package common

// DatabaseType identifies the log database engine. The primary database is
// always PostgreSQL.
type DatabaseType string

const (
	DatabaseTypePostgreSQL DatabaseType = "postgres"
	DatabaseTypeClickHouse DatabaseType = "clickhouse"
)

var logDatabaseType = DatabaseTypePostgreSQL

func LogDatabaseType() DatabaseType {
	return logDatabaseType
}

func SetLogDatabaseType(databaseType DatabaseType) {
	logDatabaseType = databaseType
}

func UsingLogDatabase(databaseType DatabaseType) bool {
	return logDatabaseType == databaseType
}
