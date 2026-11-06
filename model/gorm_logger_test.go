package model

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2/lib/proto"
	"github.com/QuantumNous/new-api/common"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 保护契约:数据库驱动错误消息可能内联数据值,非 DEBUG 下日志只保留错误码。
func TestSanitizeDBErrorStripsDriverMessage(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		want   string
		leaked string
	}{
		{
			name:   "postgres unique violation",
			err:    &pgconn.PgError{Code: "23505", Message: "duplicate key value", Detail: "Key (k)=(secret-value) already exists."},
			want:   "postgres error SQLSTATE 23505",
			leaked: "secret-value",
		},
		{
			name:   "postgres pooler prepared statement conflict gets remediation hint",
			err:    &pgconn.PgError{Severity: "FATAL", Code: "08P01", Message: "prepared statement name is already in use: stmt_secret-value"},
			want:   "postgres error SQLSTATE 08P01: prepared statement conflict with a transaction-pooling proxy (PgBouncer/Neon/Supabase); other clients sharing this database must disable prepared statements, or upgrade PgBouncer to >=1.21 with max_prepared_statements enabled",
			leaked: "secret-value",
		},
		{
			name:   "clickhouse exception",
			err:    &proto.Exception{Code: 241, Message: "Memory limit exceeded while processing 'secret-value'"},
			want:   "clickhouse error 241",
			leaked: "secret-value",
		},
		{
			name:   "wrapped driver error",
			err:    fmt.Errorf("exec failed: %w", &pgconn.PgError{Code: "42601", Message: "syntax error at or near \"secret-value\""}),
			want:   "postgres error SQLSTATE 42601",
			leaked: "secret-value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeDBError(tc.err)
			require.Error(t, got)
			assert.Equal(t, tc.want, got.Error())
			assert.NotContains(t, got.Error(), tc.leaked)
		})
	}
}

func TestSanitizeDBErrorPostgreSQLDriver(t *testing.T) {
	db := UseTestDatabases(t)
	execErr := db.Exec("INSERT INTO missing_table (k) VALUES (?)", "secret-value").Error
	require.Error(t, execErr)

	got := sanitizeDBError(execErr)
	assert.Equal(t, "postgres error SQLSTATE 42P01", got.Error())
	assert.NotContains(t, got.Error(), "secret-value")
}

func TestSanitizeDBErrorKeepsNonDriverErrors(t *testing.T) {
	err := fmt.Errorf("dial tcp 127.0.0.1:5432: connect: connection refused")
	assert.Equal(t, err, sanitizeDBError(err))
}

// 保护契约:经 gorm 真实链路,错误日志同时满足 SQL 参数化、驱动错误脱敏、
// 调用点归因到业务代码;DEBUG=true 恢复参数值与错误原文。
func TestGormLoggerEndToEndSanitizedOutput(t *testing.T) {
	previousDebug := common.DebugEnabled
	t.Cleanup(func() { common.DebugEnabled = previousDebug })

	db := UseTestDatabases(t)
	execQuery := func() string {
		var buf bytes.Buffer
		db.Session(&gorm.Session{Logger: newGormLogger(&buf)}).Exec("SELECT * FROM missing_table WHERE k = ?", "secret-value")
		return buf.String()
	}

	common.DebugEnabled = false
	out := execQuery()
	assert.Contains(t, out, "k = $1")
	assert.NotContains(t, out, "secret-value")
	assert.Contains(t, out, "postgres error SQLSTATE 42P01")
	assert.Contains(t, out, "gorm_logger_test.go")

	common.DebugEnabled = true
	debugOut := execQuery()
	assert.Contains(t, debugOut, "secret-value")
	assert.Contains(t, debugOut, `relation "missing_table" does not exist`)
}
