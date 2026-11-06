// Package testdb provisions isolated databases for backend tests.
//
// Each call creates a fresh PostgreSQL schema or ClickHouse database for one
// test and drops it when the test ends, so tests can run in parallel against a
// shared server. The package does not import the model package; callers open
// the returned DSN with model.OpenDSN.
package testdb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"

	"gorm.io/driver/clickhouse"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// T is the part of testing.TB that provisioning needs. *testing.T, *testing.B,
// and *MainScope satisfy it.
type T interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Cleanup(func())
}

// MainScope lets TestMain provision databases that live for the whole package
// run. Call Close after m.Run returns.
type MainScope struct {
	cleanups []func()
}

func (s *MainScope) Helper() {}

func (s *MainScope) Fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}

func (s *MainScope) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

func (s *MainScope) Cleanup(f func()) {
	s.cleanups = append(s.cleanups, f)
}

// Close runs the registered cleanups in reverse order.
func (s *MainScope) Close() {
	for i := len(s.cleanups) - 1; i >= 0; i-- {
		s.cleanups[i]()
	}
	s.cleanups = nil
}

const (
	// PostgresEnv names the PostgreSQL server used for the primary database.
	PostgresEnv = "TEST_POSTGRES_DSN"
	// ClickHouseLogEnv names the ClickHouse server used for the log database.
	ClickHouseLogEnv = "TEST_CLICKHOUSE_LOG_DSN"
)

// PostgresDSN creates an empty schema for t and returns a DSN whose
// search_path selects it. It returns "" when PostgresEnv is unset.
func PostgresDSN(t T) string {
	t.Helper()
	base := os.Getenv(PostgresEnv)
	if base == "" {
		return ""
	}
	name := isolatedName(t)
	admin, err := gorm.Open(postgres.New(postgres.Config{DSN: base, PreferSimpleProtocol: true}), quietConfig())
	if err != nil {
		t.Fatalf("testdb: connect to %s: %v", PostgresEnv, err)
	}
	if err := admin.Exec(fmt.Sprintf(`CREATE SCHEMA "%s"`, name)).Error; err != nil {
		t.Fatalf("testdb: create schema %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS "%s" CASCADE`, name)).Error; err != nil {
			t.Errorf("testdb: drop schema %s: %v", name, err)
		}
		closeDB(admin)
	})
	return withQueryParam(t, base, "search_path", name)
}

// ClickHouseLogDSN creates an empty ClickHouse database for t and returns a
// DSN that selects it. It returns "" when ClickHouseLogEnv is unset.
func ClickHouseLogDSN(t T) string {
	t.Helper()
	base := os.Getenv(ClickHouseLogEnv)
	if base == "" {
		return ""
	}
	name := isolatedName(t)
	admin, err := gorm.Open(clickhouse.Open(base), quietConfig())
	if err != nil {
		t.Fatalf("testdb: connect to %s: %v", ClickHouseLogEnv, err)
	}
	if err := admin.Exec(fmt.Sprintf("CREATE DATABASE `%s`", name)).Error; err != nil {
		t.Fatalf("testdb: create database %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", name)).Error; err != nil {
			t.Errorf("testdb: drop database %s: %v", name, err)
		}
		closeDB(admin)
	})
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("testdb: parse %s: %v", ClickHouseLogEnv, err)
	}
	parsed.Path = "/" + name
	return parsed.String()
}

func isolatedName(t T) string {
	t.Helper()
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("testdb: random name: %v", err)
	}
	return "t_" + hex.EncodeToString(buf)
}

func withQueryParam(t T, dsn string, key string, value string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("testdb: %s must be a URL DSN: %v", PostgresEnv, err)
	}
	query := parsed.Query()
	query.Set(key, value)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func quietConfig() *gorm.Config {
	return &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
}

func closeDB(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
