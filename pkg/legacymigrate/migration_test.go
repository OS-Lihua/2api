package legacymigrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/testdb"
	"github.com/QuantumNous/new-api/plugins"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// LEGACY_RELEASE_SQLITE is created by starting the latest published executable
// against a new SQLite database, then stopping it before adding these records.
func TestLegacyMigrationReleasedSQLite(t *testing.T) {
	released := os.Getenv("LEGACY_RELEASE_SQLITE")
	if released == "" {
		t.Skip("requires a SQLite database created by the latest released application")
	}
	testLegacyMigrationReleasedDatabase(t, "sqlite", released)
}

func TestLegacyMigrationReleasedMySQL(t *testing.T) {
	released := os.Getenv("LEGACY_RELEASE_MYSQL_URL")
	if released == "" {
		t.Skip("requires an invocation-owned MySQL database created by the latest released application")
	}
	testLegacyMigrationReleasedDatabase(t, "mysql", released)
}

func TestLegacyMigrationRejectsInvalidSourceWithoutEcho(t *testing.T) {
	const invalidSource = "mysql://fixture:fixture-not-a-real-password@127.0.0.1/example"
	var stdout, stderr bytes.Buffer
	require.Equal(t, 1, run(context.Background(), []string{"--source", invalidSource, "--backup-dir", t.TempDir()}, &stdout, &stderr))
	var report migrationReport
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, "unsupported_source", report.Failure)
	assert.Empty(t, report.Source)
	assert.False(t, bytes.Contains(stdout.Bytes(), []byte(invalidSource)))
	assert.False(t, bytes.Contains(stderr.Bytes(), []byte(invalidSource)))
}

func TestLegacyMigrationRejectsConnectionOverrides(t *testing.T) {
	// pgx accepts dbname as an override while native backup clients can select
	// the URL path instead. Reject this ambiguity before reading either database.
	target := &url.URL{Scheme: "postgres", Host: "127.0.0.1:1", Path: "/legacy", RawQuery: "dbname=actual"}
	t.Setenv("LEGACY_MIGRATION_TEST_TARGET", target.String())
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--target-env", "LEGACY_MIGRATION_TEST_TARGET", "--backup-dir", t.TempDir()}, &stdout, &stderr)
	require.Equal(t, 1, code)
	var report migrationReport
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, "target_connection_overrides_not_supported", report.Failure)
	assert.Empty(t, report.BackupDirectory, "ambiguous connections must be rejected before producing a misleading backup")
}

func TestLegacyAbilityRetirementStartup(t *testing.T) {
	for _, source := range []string{"empty_legacy_source", "lost_source", "unsealed_archive", "import_state", "staging_only", "composite_type", "collation", "text_search"} {
		for _, master := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/master=%t", source, master), func(t *testing.T) {
				t.Setenv("SQL_DSN", testdb.PostgresDSN(t))
				t.Setenv("SETTLEMENT_CURRENCY", "USD")
				previous, previousMaster := model.DB, common.IsMasterNode
				t.Cleanup(func() { model.DB, common.IsMasterNode = previous, previousMaster })
				db, _, err := model.OpenDSN("SQL_DSN", os.Getenv("SQL_DSN"), false)
				require.NoError(t, err)
				pool, err := db.DB()
				require.NoError(t, err)
				t.Cleanup(func() { _ = pool.Close() })
				switch source {
				case "empty_legacy_source":
					require.NoError(t, db.AutoMigrate(&model.Ability{}))
				case "lost_source":
					require.NoError(t, db.Exec(`CREATE TABLE application_evidence (id bigint); INSERT INTO application_evidence VALUES (9)`).Error)
				case "unsealed_archive":
					require.NoError(t, db.AutoMigrate(&model.LegacyAbilitySource{}))
				case "composite_type":
					require.NoError(t, db.Exec(`CREATE TYPE application_record AS (id bigint)`).Error)
				case "collation":
					require.NoError(t, db.Exec(`CREATE COLLATION application_order FROM pg_catalog."C"`).Error)
				case "text_search":
					require.NoError(t, db.Exec(`CREATE TEXT SEARCH CONFIGURATION application_search (COPY = pg_catalog.english)`).Error)
				case "staging_only":
					require.NoError(t, db.Exec(`CREATE SCHEMA legacy_import_retirement_fixture; CREATE TABLE legacy_import_retirement_fixture.imported_record (id bigint)`).Error)
					t.Cleanup(func() { require.NoError(t, db.Exec(`DROP SCHEMA legacy_import_retirement_fixture CASCADE`).Error) })
				case "import_state":
					t.Cleanup(func() { require.NoError(t, db.Exec(`DROP SCHEMA gateway_migration CASCADE`).Error) })
					require.NoError(t, db.Exec(`CREATE SCHEMA gateway_migration; CREATE TABLE gateway_migration.source_imports (id bigint, phase text); INSERT INTO gateway_migration.source_imports VALUES (1,'loading')`).Error)
				}
				inventory := func() string {
					var state string
					require.NoError(t, db.Raw(`SELECT COALESCE(string_agg(n.nspname||'.'||c.relname,',' ORDER BY n.nspname,c.relname),'') FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema' AND c.relkind IN ('r','p','S','v','m')`).Scan(&state).Error)
					return state
				}
				before := inventory()
				common.IsMasterNode = master
				for range 2 {
					startupErr := model.InitDB()
					if source == "empty_legacy_source" {
						assert.ErrorIs(t, startupErr, model.ErrLegacyAbilityOffline)
					} else {
						assert.ErrorIs(t, startupErr, model.ErrLegacyAbilityRetirement)
					}
					if model.DB != nil {
						opened, err := model.DB.DB()
						require.NoError(t, err)
						_ = opened.Close()
					}
					assert.Equal(t, before, inventory())
				}
			})
		}
	}
	t.Run("fresh_slave_never_initializes", func(t *testing.T) {
		t.Setenv("SQL_DSN", testdb.PostgresDSN(t))
		t.Setenv("SETTLEMENT_CURRENCY", "USD")
		previous, previousMaster := model.DB, common.IsMasterNode
		t.Cleanup(func() { model.DB, common.IsMasterNode = previous, previousMaster })
		db, _, err := model.OpenDSN("SQL_DSN", os.Getenv("SQL_DSN"), false)
		require.NoError(t, err)
		pool, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		common.IsMasterNode = false
		for range 2 {
			assert.ErrorIs(t, model.InitDB(), model.ErrNativeDatabaseMaster)
			opened, err := model.DB.DB()
			require.NoError(t, err)
			_ = opened.Close()
			var tables int64
			require.NoError(t, db.Raw(`SELECT count(*) FROM pg_class WHERE relnamespace=current_schema()::regnamespace`).Scan(&tables).Error)
			assert.Zero(t, tables)
		}
	})
	t.Run("fresh_never_creates_source", func(t *testing.T) {
		db := nativeMigrationDB(t)
		assert.False(t, db.Migrator().HasTable("abilities"))
		assert.True(t, db.Migrator().HasTable("legacy_ability_retirements"))
		assert.False(t, db.Migrator().HasTable("legacy_ability_sources"))
	})
}

func TestLegacyMigrationFreshStartup(t *testing.T) {
	dsn := testdb.PostgresDSN(t)
	require.NotEmpty(t, dsn, "requires an invocation-owned real PostgreSQL database")
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	previous, previousMaster := model.DB, common.IsMasterNode
	t.Cleanup(func() { model.DB, common.IsMasterNode = previous, previousMaster })
	common.IsMasterNode = true
	for start := range 2 {
		require.NoError(t, model.InitDB())
		pool, err := model.DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		if start == 0 {
			require.NoError(t, model.DB.Create(&model.User{Id: 91, Username: "migration-fresh", Password: "fixture-password-hash", AffCode: "migration-fresh-aff"}).Error)
			require.NoError(t, model.DB.Create(&model.Token{Id: 91, UserId: 91, Key: "fixture-fresh-key", ModelLimitsEnabled: true, ModelLimits: "unknown-model"}).Error)
		}
	}
	var policy model.TokenOfferingPolicy
	require.NoError(t, model.DB.Where("token_id = ?", 91).First(&policy).Error)
	assert.False(t, policy.DefaultAllow, "moving the offline migration seam must retain normal startup's permission migration")
}

// This explicit test-only schema represents a pre-retirement installation. A
// native deployment must never create these sources or remove its proof.
func declareLegacyAbilityFixture(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec(`DROP TABLE legacy_ability_retirements; DROP FUNCTION protect_legacy_ability_retirement()`).Error)
	require.NoError(t, db.AutoMigrate(&model.Ability{}))
}

func midjourneyMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := nativeMigrationDB(t)
	declareLegacyAbilityFixture(t, db)
	return db
}

func nativeMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	t.Setenv("SQL_DSN", testdb.PostgresDSN(t))
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	previous, master := model.DB, common.IsMasterNode
	t.Cleanup(func() { model.DB, common.IsMasterNode = previous, master })
	common.IsMasterNode = true
	require.NoError(t, model.InitDB())
	db := model.DB
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

func midjourneyMigrationState(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, table := range []string{"midjourneys", "tasks", "legacy_gateway_migrations", "legacy_task_obligations", "wallets", "ledger_entries", "gateway_billings"} {
		var digest string
		require.NoError(t, db.Raw(fmt.Sprintf(`SELECT md5(COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text)::text,'')) FROM %q t`, table)).Scan(&digest).Error)
		state[table] = digest
	}
	return state
}

// Capture only in memory: a failing secrecy assertion must not print or archive
// the captured stream, which might contain the very private value under test.
func captureMidjourneyValidationLogs(t *testing.T, action func()) (captured string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	originalOut, originalErr := os.Stdout, os.Stderr
	common.LogWriterMu.Lock()
	originalGin, originalGinErr := gin.DefaultWriter, gin.DefaultErrorWriter
	gin.DefaultWriter, gin.DefaultErrorWriter = writer, writer
	common.LogWriterMu.Unlock()
	os.Stdout, os.Stderr = writer, writer
	finished := make(chan string, 1)
	go func() {
		var output bytes.Buffer
		_, _ = io.Copy(&output, reader)
		_ = reader.Close()
		finished <- output.String()
	}()
	defer func() {
		os.Stdout, os.Stderr = originalOut, originalErr
		common.LogWriterMu.Lock()
		gin.DefaultWriter, gin.DefaultErrorWriter = originalGin, originalGinErr
		common.LogWriterMu.Unlock()
		_ = writer.Close()
		captured = <-finished
	}()
	action()
	return ""
}

func TestLegacyTaskOpaqueJSONStartupAndOffline(t *testing.T) {
	db := midjourneyMigrationDB(t).Session(&gorm.Session{Logger: logger.Discard})
	source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "opaque-original-upstream", Action: "IMAGINE", Quota: 1200, Status: "QUEUED"}
	require.NoError(t, db.Create(&source).Error)
	_, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	require.NoError(t, db.First(&source, source.Id).Error)
	require.NoError(t, db.Create(&model.User{Id: 41, Username: "opaque-json-owner", Status: 1, AffCode: "opaque-aff"}).Error)
	_, err = model.TopUpWallet(model.WalletTopUp{UserId: 41, Source: money.Amount{Nanos: 1_000_000_001, Currency: "USD"}, Reference: "opaque-fixture-opening"})
	require.NoError(t, err)
	const canary = "opaque-startup-private-value-must-not-leak"
	for i, kind := range []string{"ordinary", "subscription", "gateway"} {
		private := model.TaskPrivateData{BillingSource: "wallet", TokenId: 71, UpstreamTaskID: "opaque-task-upstream", Key: canary,
			PluginState: common.RawMessage(`{"n":1e200000}`),
			Execution:   &model.TaskExecutionSnapshot{RequestID: "opaque-request", TaskPlugin: &model.TaskPluginSnapshot{Key: "sora", Version: "1.2.3", Generation: math.MaxUint64}}}
		if kind != "ordinary" {
			private.BillingSource, private.SubscriptionId = "subscription", 991
		}
		if kind == "gateway" {
			private.Gateway = &model.TaskGatewaySnapshot{}
		}
		encoded, err := common.Marshal(private)
		require.NoError(t, err)
		raw := strings.TrimSuffix(string(encoded), "}") + `,"future":{"n":1e200000}}`
		task := model.Task{ID: int64(901 + i), TaskID: "opaque-" + kind, UserId: 41, ChannelId: 991, Quota: 9_007_199_254_740_993, Status: model.TaskStatusSuccess}
		require.NoError(t, db.Create(&task).Error)
		require.NoError(t, db.Exec(`UPDATE tasks SET private_data=?::json WHERE id=?`, raw, task.ID).Error)
	}
	require.NoError(t, db.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||
 ',"future":{"n":1e200000},"plugin_state":{"n":1e200000},"opaque_secret":"'||?||'"}')::json WHERE id=?`, canary, source.MigratedTaskID).Error)
	var readable []model.Task
	require.NoError(t, db.Find(&readable).Error, "actual Task.Scan accepts every opaque source")
	require.NoError(t, model.ValidateLegacyMidjourneyMigration(db), "the shared MJ identity and decoder contract is unchanged")
	state := func(tables ...string) map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, table := range tables {
			var digest string
			require.NoError(t, db.Raw(fmt.Sprintf(`SELECT md5(COALESCE(string_agg(row_to_json(t)::text,E'\n' ORDER BY row_to_json(t)::text),'')) FROM %q t`, table)).Scan(&digest).Error)
			result[table] = digest
		}
		return result
	}
	tables := []string{"midjourneys", "tasks", "users", "legacy_gateway_migrations", "wallets", "ledger_entries", "gateway_billings", "user_subscriptions", "subscription_periods"}
	before := state(tables...)
	var captured map[string]string
	check := func() {
		t.Helper()
		assert.Equal(t, before, state(tables...), "no source-byte, identity or financial rewrite")
		var ordinary []model.LegacyTaskObligation
		var subscription []model.LegacySubscriptionTaskObligation
		require.NoError(t, db.Find(&ordinary).Error)
		require.NoError(t, db.Find(&subscription).Error)
		require.Len(t, ordinary, 1)
		require.Len(t, subscription, 1)
		assert.EqualValues(t, 901, ordinary[0].SourceTaskID)
		assert.Equal(t, uint64(math.MaxUint64), ordinary[0].PluginGeneration)
		assert.EqualValues(t, 902, subscription[0].SourceTaskID)
		assert.Equal(t, 991, subscription[0].SubscriptionID, "missing historical parents remain unverified evidence")
		assert.Equal(t, "unverified", ordinary[0].State)
		assert.Equal(t, "unverified", subscription[0].State)
		after := state("legacy_task_obligations", "legacy_subscription_task_obligations")
		if captured != nil {
			assert.Equal(t, captured, after, "both immutable evidence archives remain identical on replay")
		}
		captured = after
	}
	debug := common.DebugEnabled
	common.DebugEnabled = true
	t.Cleanup(func() { common.DebugEnabled = debug })
	var queryLog bytes.Buffer
	logged := db.Session(&gorm.Session{Logger: logger.New(log.New(&queryLog, "", 0), logger.Config{LogLevel: logger.Info})})
	logs := captureMidjourneyValidationLogs(t, func() {
		for _, master := range []bool{true, false} {
			common.IsMasterNode = master
			for range 2 {
				require.NoError(t, model.InitDB())
				pool, err := model.DB.DB()
				require.NoError(t, err)
				t.Cleanup(func() { _ = pool.Close() })
				check()
			}
		}
		model.DB = logged
		for range 2 {
			report, err := model.MigrateLegacyGateway(t.Context())
			require.NoError(t, err)
			require.True(t, report.Committed)
			require.True(t, report.AlreadyCompleted)
			encoded, err := common.Marshal(report)
			require.NoError(t, err)
			assert.False(t, bytes.Contains(encoded, []byte(canary)))
			check()
		}
	})
	for _, text := range []string{logs, queryLog.String()} {
		assert.False(t, strings.Contains(text, canary), "private values never appear in stdout, stderr or query logs")
		assert.False(t, strings.Contains(text, "1e200000"), "opaque values never appear in diagnostic output")
	}
}

func TestLegacyMidjourneyRejectsUnreadablePrivateData(t *testing.T) {
	for _, name := range []string{"result_url_number", "completion_facts_string", "completion_number_overflow", "key_canary"} {
		t.Run(name, func(t *testing.T) {
			db := midjourneyMigrationDB(t)
			source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "typed-decode-upstream", Action: "IMAGINE", Quota: 1200, Status: "SUCCESS", Progress: "100%"}
			require.NoError(t, db.Create(&source).Error)
			_, err := model.MigrateLegacyGateway(t.Context())
			require.NoError(t, err)
			require.NoError(t, db.First(&source, source.Id).Error)
			const canary = "fixture-private-decode-canary-never-log"
			quiet := db.Session(&gorm.Session{Logger: logger.Discard})
			require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{private_decode_canary}',to_jsonb(?::text))::json WHERE id=?`, canary, source.MigratedTaskID).Error)
			switch name {
			case "key_canary":
				require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(jsonb_set(private_data::jsonb,'{key}',to_jsonb(?::text)),'{result_url}','17')::json WHERE id=?`, canary, source.MigratedTaskID).Error)
			case "result_url_number":
				require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{result_url}','17')::json WHERE id=?`, source.MigratedTaskID).Error)
			case "completion_facts_string":
				require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,completion_facts}','"not-an-object"')::json WHERE id=?`, source.MigratedTaskID).Error)
			case "completion_number_overflow":
				require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,completion_facts}','{"output_tokens":1e400}')::json WHERE id=?`, source.MigratedTaskID).Error)
			}
			var unreadable model.Task
			assert.Error(t, quiet.First(&unreadable, source.MigratedTaskID).Error, "the real GORM Task decoder must reject this fixture")
			before := midjourneyMigrationState(t, quiet)
			var queryLogs bytes.Buffer
			logged := db.Session(&gorm.Session{Logger: logger.New(log.New(&queryLogs, "", 0), logger.Config{LogLevel: logger.Info})})
			previousDebug := common.DebugEnabled
			common.DebugEnabled = true
			t.Cleanup(func() { common.DebugEnabled = previousDebug })
			captured := captureMidjourneyValidationLogs(t, func() {
				assert.ErrorIs(t, model.ValidateLegacyMidjourneyMigration(logged), model.ErrLegacyMidjourneyMigrationRequired)
				for _, master := range []bool{true, false} {
					common.IsMasterNode = master
					assert.ErrorIs(t, model.InitDB(), model.ErrLegacyMidjourneyMigrationRequired, "startup must reject an unreadable actual mapping target")
					pool, err := model.DB.DB()
					require.NoError(t, err)
					t.Cleanup(func() { _ = pool.Close() })
					assert.Equal(t, before, midjourneyMigrationState(t, quiet))
				}
				model.DB = logged
				for range 2 {
					report, err := model.MigrateLegacyGateway(t.Context())
					assert.ErrorIs(t, err, model.ErrLegacyGatewayMigrationFailed)
					assert.False(t, report.Committed)
					encoded, marshalErr := common.Marshal(report)
					require.NoError(t, marshalErr)
					assert.False(t, bytes.Contains(encoded, []byte(canary)), "report must not expose the private canary")
					assert.Equal(t, before, midjourneyMigrationState(t, quiet), "do not repair, normalize or discard unreadable private fields")
				}
			})
			for _, output := range []string{captured, queryLogs.String()} {
				assert.False(t, strings.Contains(output, canary), "validation logs must not contain the private canary")
				assert.False(t, strings.Contains(output, "cannot unmarshal"), "validation logs must not expose raw JSON decoding errors")
			}
		})
	}
}

func TestLegacyMidjourneyPrivateDataResourceBound(t *testing.T) {
	const limit = 16 << 20
	db := midjourneyMigrationDB(t)
	quiet := db.Session(&gorm.Session{Logger: logger.Discard})
	source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "bounded-private-upstream", Action: "IMAGINE", Quota: 1200}
	require.NoError(t, quiet.Create(&source).Error)
	_, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	require.NoError(t, quiet.First(&source, source.Id).Error)
	require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=(private_data::jsonb ||
 '{"unknown_valid":{"future":1e400},"plugin_state":{"state":"kept","future":1e400},"padding":""}'::jsonb)::json WHERE id=?`, source.MigratedTaskID).Error)
	var baseSize int
	require.NoError(t, quiet.Raw(`SELECT octet_length(private_data::text) FROM tasks WHERE id=?`, source.MigratedTaskID).Scan(&baseSize).Error)
	setSize := func(size int) {
		t.Helper()
		require.NoError(t, quiet.Exec(`UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{padding}',to_jsonb(repeat('x',?)))::json WHERE id=?`, size-baseSize, source.MigratedTaskID).Error)
		var actual int
		require.NoError(t, quiet.Raw(`SELECT octet_length(private_data::text) FROM tasks WHERE id=?`, source.MigratedTaskID).Scan(&actual).Error)
		require.Equal(t, size, actual)
	}
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprintf("oversized=%v", oversized), func(t *testing.T) {
			size := limit
			if oversized {
				size++
			}
			setSize(size)
			var readable model.Task
			require.NoError(t, quiet.First(&readable, source.MigratedTaskID).Error, "the byte limit is resource protection, not a claim of corrupt JSON")
			require.Contains(t, string(readable.PrivateData.PluginState), `"kept"`)
			before := midjourneyMigrationState(t, quiet)
			var keyQueries, rawQueries int
			callback := "test:mj_private_resource_order"
			require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callback, func(query *gorm.DB) {
				statement := query.Statement.SQL.String()
				if strings.Contains(statement, "AS private_keys") {
					keyQueries++
				}
				if strings.Contains(statement, "AS typed_private_data") {
					rawQueries++
				}
			}))
			t.Cleanup(func() { _ = db.Callback().Row().Remove(callback) })
			check := func(err error) {
				t.Helper()
				if oversized {
					require.ErrorIs(t, err, model.ErrLegacyMidjourneyMigrationRequired)
					require.Contains(t, err.Error(), "resource limit")
				} else {
					require.NoError(t, err, "unknown valid fields and RawMessage PluginState keep normal Go decoding semantics")
				}
			}
			check(model.ValidateLegacyMidjourneyMigration(db))
			if oversized {
				assert.Zero(t, keyQueries, "size rejection must precede key-name aggregation/transfer")
				assert.Zero(t, rawQueries, "oversized raw private data must never be selected")
			} else {
				assert.Positive(t, keyQueries)
				assert.Positive(t, rawQueries)
			}
			for _, master := range []bool{true, false} {
				common.IsMasterNode = master
				check(model.InitDB())
				pool, err := model.DB.DB()
				require.NoError(t, err)
				t.Cleanup(func() { _ = pool.Close() })
				assert.Equal(t, before, midjourneyMigrationState(t, quiet))
			}
			model.DB = db
			for range 2 {
				report, err := model.MigrateLegacyGateway(t.Context())
				if oversized {
					require.ErrorIs(t, err, model.ErrLegacyGatewayMigrationFailed)
					assert.False(t, report.Committed)
				} else {
					require.NoError(t, err)
					assert.True(t, report.Committed)
					assert.True(t, report.AlreadyCompleted)
				}
				assert.Equal(t, before, midjourneyMigrationState(t, quiet), "keep source, mapped Task, evidence and financial rows unchanged")
			}
		})
	}
	// The bound belongs to legacy identity validation, not unrelated Task state.
	setSize(baseSize)
	require.NoError(t, quiet.Exec(`INSERT INTO tasks (id,task_id,private_data)
 VALUES (9999,'unrelated-large-task',json_build_object('padding',repeat('y',?)))`, limit+1).Error)
	require.NoError(t, model.ValidateLegacyMidjourneyMigration(db))
}

func TestLegacyMidjourneyRejectsJSONIdentityAliases(t *testing.T) {
	db := midjourneyMigrationDB(t)
	source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "original-upstream", Action: "IMAGINE", Status: "QUEUED"}
	require.NoError(t, db.Create(&source).Error)
	_, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	require.NoError(t, db.First(&source, source.Id).Error)
	for _, kind := range []string{"upper_upstream", "unicode_upstream", "duplicate_upstream", "upper_gateway", "unicode_key", "upper_legacy_claim", "upper_source_id", "numeric_source", "string_subscription", "forged_token"} {
		t.Run(kind, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			t.Cleanup(func() { _ = tx.Rollback().Error })
			switch kind {
			case "upper_upstream", "unicode_upstream", "duplicate_upstream":
				key := map[string]string{"upper_upstream": "UPSTREAM_TASK_ID", "unicode_upstream": "UPſTREAM_TASK_ID", "duplicate_upstream": "upstream_task_id"}[kind]
				suffix := fmt.Sprintf(`,%q:"other"}`, key)
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||?)::json WHERE id=?`, suffix, source.MigratedTaskID).Error)
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				require.Equal(t, "other", task.PrivateData.UpstreamTaskID, "prove the actual Go decoder accepts the alias")
			case "upper_gateway":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||',"Gateway":{}}')::json WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				require.NotNil(t, task.PrivateData.Gateway)
			case "unicode_key":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||',"Key":"fixture-identity-secret"}')::json WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				require.Equal(t, "fixture-identity-secret", task.PrivateData.Key)
			case "upper_legacy_claim":
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				task.ID, task.TaskID = 0, "another-public-task"
				require.NoError(t, tx.Create(&task).Error)
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=json_build_object('Legacy_Obligation',private_data::json->'legacy_obligation') WHERE id=?`, task.ID).Error)
				task = model.Task{ID: task.ID}
				require.NoError(t, tx.First(&task).Error)
				require.NotNil(t, task.PrivateData.LegacyObligation)
				require.Equal(t, source.Id, task.PrivateData.LegacyObligation.SourceID)
			case "numeric_source":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=replace(private_data::text,'"source_id":91','"source_id":91e0')::json WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				assert.Error(t, tx.Session(&gorm.Session{Logger: logger.Discard}).First(&task, source.MigratedTaskID).Error, "Go refuses exponent notation for an int64 source identity")
			case "string_subscription":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||',"subscription_id":"0"}')::json WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				assert.Error(t, tx.Session(&gorm.Session{Logger: logger.Discard}).First(&task, source.MigratedTaskID).Error, "Go refuses a string subscription identity")
			case "forged_token":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=(LEFT(private_data::text,LENGTH(private_data::text)-1)||',"token_id":77}')::json WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				assert.Equal(t, 77, task.PrivateData.TokenId, "the existing MJ import does not freeze a Token identity")
			case "upper_source_id":
				require.NoError(t, tx.Exec(`UPDATE tasks SET private_data=json_build_object('upstream_task_id',private_data::json->'upstream_task_id','legacy_obligation',(LEFT((private_data::json->'legacy_obligation')::text,LENGTH((private_data::json->'legacy_obligation')::text)-1)||',"SOURCE_ID":92}')::json) WHERE id=?`, source.MigratedTaskID).Error)
				var task model.Task
				require.NoError(t, tx.First(&task, source.MigratedTaskID).Error)
				require.Equal(t, 92, task.PrivateData.LegacyObligation.SourceID)
			}
			before := midjourneyMigrationState(t, tx)
			assert.Error(t, model.ValidateLegacyMidjourneyMigration(tx), "SQL must not accept an identity interpreted differently by the Go decoder")
			previous := model.DB
			model.DB = tx
			t.Cleanup(func() { model.DB = previous })
			report, err := model.MigrateLegacyGateway(t.Context())
			assert.Error(t, err, "offline migration must use the same identity semantics")
			assert.False(t, report.Committed)
			assert.Equal(t, before, midjourneyMigrationState(t, tx))
		})
	}
}

func TestLegacyMidjourneyStartupRequiresOfflineMigration(t *testing.T) {
	for _, master := range []bool{true, false} {
		t.Run(fmt.Sprintf("master_%t", master), func(t *testing.T) {
			db := nativeMigrationDB(t)
			source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "legacy-upstream", Action: "IMAGINE", Quota: 1200, Status: "SUCCESS", Progress: "100%"}
			require.NoError(t, db.Create(&source).Error)
			common.IsMasterNode = master
			for range 2 {
				err := model.InitDB()
				assert.Error(t, err, "all real legacy rows require offline migration, including terminal rows and non-master startup")
				pool, poolErr := model.DB.DB()
				require.NoError(t, poolErr)
				t.Cleanup(func() { _ = pool.Close() })
				var stored model.Midjourney
				require.NoError(t, db.First(&stored, source.Id).Error)
				assert.Equal(t, source, stored, "startup must never import or rewrite the source")
				for _, table := range []string{"tasks", "wallets", "ledger_entries", "gateway_billings"} {
					var count int64
					require.NoError(t, db.Table(table).Count(&count).Error)
					assert.Zero(t, count, table)
				}
			}
		})
	}
}

func TestLegacyMidjourneyFreshAndReadFailures(t *testing.T) {
	for _, master := range []bool{true, false} {
		t.Run(fmt.Sprintf("empty_released_schema_master_%t", master), func(t *testing.T) {
			db := nativeMigrationDB(t)
			require.NoError(t, db.Exec(`ALTER TABLE midjourneys DROP COLUMN migrated_task_id`).Error)
			common.IsMasterNode = master
			require.NoError(t, model.InitDB(), "an empty released table does not require new columns or a marker")
			pool, err := model.DB.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			assert.Equal(t, master, db.Migrator().HasColumn(&model.Midjourney{}, "migrated_task_id"))
		})
	}
	t.Run("nonempty_missing_column", func(t *testing.T) {
		db := midjourneyMigrationDB(t)
		require.NoError(t, db.Create(&model.Midjourney{Id: 91, Status: "SUCCESS", Progress: "100%"}).Error)
		require.NoError(t, db.Exec(`ALTER TABLE midjourneys DROP COLUMN migrated_task_id`).Error)
		for _, master := range []bool{true, false} {
			common.IsMasterNode = master
			assert.Error(t, model.InitDB())
			pool, err := model.DB.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			assert.False(t, db.Migrator().HasColumn(&model.Midjourney{}, "migrated_task_id"), "gate must fail before schema writes")
		}
	})
	t.Run("missing_table_and_query_errors", func(t *testing.T) {
		db := midjourneyMigrationDB(t)
		require.NoError(t, db.Exec(`DROP TABLE midjourneys`).Error)
		require.NoError(t, model.ValidateLegacyMidjourneyMigration(db))
		const callback = "test:midjourney-source-read-error"
		require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
			tx.AddError(errors.New("fixture-storage-error-not-for-logs"))
		}))
		t.Cleanup(func() { _ = db.Callback().Row().Remove(callback) })
		err := model.ValidateLegacyMidjourneyMigration(db)
		require.Error(t, err, "failed existence query is not an absent table")
		assert.NotContains(t, err.Error(), "fixture-storage-error-not-for-logs")
		require.NoError(t, db.Callback().Row().Remove(callback))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assert.Error(t, model.ValidateLegacyMidjourneyMigration(db.WithContext(ctx)))
	})
}

func TestLegacyMidjourneyIncrementalAfterMarker(t *testing.T) {
	db := midjourneyMigrationDB(t)
	first := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "first-upstream", Action: "IMAGINE", Quota: 1200, Status: "QUEUED", Progress: "0%"}
	require.NoError(t, db.Create(&first).Error)
	report, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	require.True(t, report.Committed)
	require.EqualValues(t, 1, report.Counts["midjourney_obligations"])
	require.NoError(t, db.First(&first, first.Id).Error)
	var original string
	require.NoError(t, db.Raw(`SELECT to_jsonb(t)::text FROM tasks t WHERE id=?`, first.MigratedTaskID).Scan(&original).Error)
	late := model.Midjourney{Id: 92, UserId: 42, ChannelId: 992, MjId: "late-upstream", Action: "UPSCALE", Quota: -1, Status: "SUCCESS", Progress: "100%"}
	require.NoError(t, db.Create(&late).Error)
	report, err = model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	assert.True(t, report.Committed)
	assert.False(t, report.AlreadyCompleted, "a marker does not prove that a late source row was imported")
	assert.EqualValues(t, 1, report.Counts["midjourney_obligations"])
	require.NoError(t, db.First(&late, late.Id).Error)
	require.Positive(t, late.MigratedTaskID)
	var task model.Task
	require.NoError(t, db.First(&task, late.MigratedTaskID).Error)
	require.NotNil(t, task.PrivateData.LegacyObligation)
	assert.Zero(t, task.PrivateData.LegacyObligation.ProviderType, "missing Provider cannot acquire invented polling authority")
	assert.Equal(t, -1, task.PrivateData.LegacyObligation.SourceQuota)
	for range 2 {
		report, err = model.MigrateLegacyGateway(t.Context())
		require.NoError(t, err)
		assert.True(t, report.AlreadyCompleted)
		assert.Zero(t, report.Counts["midjourney_obligations"])
		var after string
		require.NoError(t, db.Raw(`SELECT to_jsonb(t)::text FROM tasks t WHERE id=?`, first.MigratedTaskID).Scan(&after).Error)
		assert.JSONEq(t, original, after)
	}
	var count int64
	require.NoError(t, db.Model(&model.Task{}).Count(&count).Error)
	assert.EqualValues(t, 2, count)
	require.NoError(t, db.Model(&model.LedgerEntry{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestLegacyMidjourneyRejectsBrokenMappingsAtomically(t *testing.T) {
	db := midjourneyMigrationDB(t)
	source := model.Midjourney{Id: 91, UserId: 41, ChannelId: 991, MjId: "retained-upstream", Action: "IMAGINE", Quota: 1200, Status: "QUEUED"}
	require.NoError(t, db.Create(&source).Error)
	_, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	require.NoError(t, db.First(&source, source.Id).Error)
	cases := map[string]string{
		"missing_target":         `DELETE FROM tasks WHERE id=?`,
		"wrong_owner":            `UPDATE tasks SET user_id=42 WHERE id=?`,
		"wrong_provider":         `UPDATE tasks SET channel_id=992 WHERE id=?`,
		"wrong_action":           `UPDATE tasks SET action='UPSCALE' WHERE id=?`,
		"wrong_platform":         `UPDATE tasks SET platform='sora' WHERE id=?`,
		"invented_quota":         `UPDATE tasks SET quota=1 WHERE id=?`,
		"public_identity_alias":  `UPDATE tasks SET task_id='retained-upstream' WHERE id=?`,
		"wrong_upstream":         `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{upstream_task_id}','"changed"')::json WHERE id=?`,
		"wrong_source_id":        `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,source_id}','90')::json WHERE id=?`,
		"wrong_source_provider":  `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,provider_id}','992')::json WHERE id=?`,
		"wrong_source_quota":     `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,source_quota}','0')::json WHERE id=?`,
		"invented_provider_type": `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,provider_type}','1')::json WHERE id=?`,
		"wrong_version":          `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,version}','"unknown"')::json WHERE id=?`,
		"invented_paid_fact":     `UPDATE tasks SET private_data=jsonb_set(private_data::jsonb,'{legacy_obligation,debit_evidence}','"verified"')::json WHERE id=?`,
		"missing_obligation":     `UPDATE tasks SET private_data=(private_data::jsonb-'legacy_obligation')::json WHERE id=?`,
		"duplicate_public_id":    `INSERT INTO tasks (task_id) SELECT task_id FROM tasks WHERE id=?`,
		"duplicate_claim":        `INSERT INTO tasks (task_id,platform,user_id,channel_id,quota,action,private_data) SELECT 'duplicate-public-id',platform,user_id,channel_id,quota,action,private_data FROM tasks WHERE id=?`,
		"lost_mapping":           `UPDATE midjourneys SET migrated_task_id=0 WHERE migrated_task_id=?`,
		"wrong_mapping":          `UPDATE midjourneys SET migrated_task_id=987654 WHERE migrated_task_id=?`,
	}
	for name, statement := range cases {
		t.Run(name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			previous := model.DB
			model.DB = tx
			t.Cleanup(func() { model.DB = previous; _ = tx.Rollback().Error })
			require.NoError(t, tx.Exec(statement, source.MigratedTaskID).Error)
			require.NoError(t, tx.Create(&model.Midjourney{Id: 92, MjId: "late-valid-source", Status: "SUCCESS", Progress: "100%"}).Error)
			before := midjourneyMigrationState(t, tx)
			assert.Error(t, model.ValidateLegacyMidjourneyMigration(tx))
			report, err := model.MigrateLegacyGateway(t.Context())
			assert.Error(t, err)
			assert.False(t, report.Committed)
			assert.Equal(t, before, midjourneyMigrationState(t, tx), "refusal cannot repair mappings, overwrite evidence or partly import new rows")
		})
	}
	t.Run("later_write_failure", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		previous := model.DB
		model.DB = tx
		t.Cleanup(func() { model.DB = previous; _ = tx.Rollback().Error })
		for _, id := range []int{92, 93} {
			require.NoError(t, tx.Create(&model.Midjourney{Id: id, MjId: fmt.Sprintf("late-%d", id)}).Error)
		}
		require.NoError(t, tx.Exec(`CREATE FUNCTION reject_second_mj_mapping() RETURNS trigger AS $$ BEGIN IF NEW.id=93 THEN RAISE EXCEPTION 'fixture mapping write failure'; END IF; RETURN NEW; END; $$ LANGUAGE plpgsql;
CREATE TRIGGER reject_second_mj_mapping BEFORE UPDATE ON midjourneys FOR EACH ROW EXECUTE FUNCTION reject_second_mj_mapping()`).Error)
		before := midjourneyMigrationState(t, tx)
		report, err := model.MigrateLegacyGateway(t.Context())
		require.Error(t, err)
		assert.False(t, report.Committed)
		assert.EqualValues(t, 1, report.Counts["midjourney_obligations"], "the first new row was attempted before the later write failure")
		assert.Equal(t, before, midjourneyMigrationState(t, tx), "all attempted inserts and mapping writes must roll back")
	})
}

func TestLegacyTaskObligationStartup(t *testing.T) {
	for _, mode := range []string{"normal_startup", "completed_offline_migration"} {
		t.Run(mode, func(t *testing.T) {
			dsn := testdb.PostgresDSN(t)
			require.NotEmpty(t, dsn)
			t.Setenv("SQL_DSN", dsn)
			t.Setenv("SETTLEMENT_CURRENCY", "USD")
			previousDB, previousMaster := model.DB, common.IsMasterNode
			t.Cleanup(func() { model.DB, common.IsMasterNode = previousDB, previousMaster })
			common.IsMasterNode = true
			require.NoError(t, model.InitDB())
			db := model.DB
			pool, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			if mode == "completed_offline_migration" {
				declareLegacyAbilityFixture(t, db)
				report, err := model.MigrateLegacyGateway(t.Context())
				require.NoError(t, err)
				require.True(t, report.Committed)
			}
			// Deleted parent records cannot erase a previously persisted task.
			task := model.Task{
				ID: 701, UserId: 991, ChannelId: 992, TaskID: "retained-task", Platform: "sora",
				Quota: 2500, Status: model.TaskStatusInProgress, CreatedAt: 1700000000, UpdatedAt: 1700000010,
				SubmitTime: 1700000001, StartTime: 1700000002,
				PrivateData: model.TaskPrivateData{
					BillingSource: "wallet", TokenId: 993, UpstreamTaskID: "retained-upstream-task", Key: "fixture-not-a-usable-key",
					Execution: &model.TaskExecutionSnapshot{RequestID: "retained-request", TaskPlugin: &model.TaskPluginSnapshot{
						Key: "sora", Version: "retained-version", Generation: math.MaxUint64,
					}},
				},
			}
			require.NoError(t, db.Create(&task).Error)
			var before string
			require.NoError(t, db.Raw(`SELECT to_jsonb(t)::text FROM "tasks" t WHERE "id"=701`).Scan(&before).Error)
			var firstEvidence string
			for pass := range 2 {
				if mode == "normal_startup" {
					require.NoError(t, model.InitDB())
					connection, err := model.DB.DB()
					require.NoError(t, err)
					t.Cleanup(func() { _ = connection.Close() })
				} else {
					report, err := model.MigrateLegacyGateway(t.Context())
					require.NoError(t, err)
					assert.True(t, report.Committed)
					assert.True(t, report.AlreadyCompleted)
				}
				require.True(t, db.Migrator().HasTable("legacy_task_obligations"), "startup and completed migration must independently archive historical task obligations")
				var evidence struct {
					SourceTaskID, SourceQuota                               int64
					UserID, ProviderID, TokenID                             int
					TaskID, UpstreamTaskID, RequestID, BillingSource, State string
					PluginKey, PluginVersion, PluginGeneration              string
				}
				require.NoError(t, db.Table("legacy_task_obligations").Where("source_task_id = ?", task.ID).Take(&evidence).Error)
				assert.Equal(t, task.ID, evidence.SourceTaskID)
				assert.Equal(t, 991, evidence.UserID)
				assert.Equal(t, 992, evidence.ProviderID)
				assert.Equal(t, 993, evidence.TokenID)
				assert.Equal(t, "retained-task", evidence.TaskID)
				assert.Equal(t, "retained-upstream-task", evidence.UpstreamTaskID)
				assert.Equal(t, "retained-request", evidence.RequestID)
				assert.Equal(t, "wallet", evidence.BillingSource)
				assert.EqualValues(t, 2500, evidence.SourceQuota)
				assert.Equal(t, "unverified", evidence.State)
				assert.Equal(t, "sora", evidence.PluginKey)
				assert.Equal(t, "retained-version", evidence.PluginVersion)
				assert.Equal(t, "18446744073709551615", evidence.PluginGeneration)
				var sourceAfter, evidenceJSON string
				require.NoError(t, db.Raw(`SELECT to_jsonb(t)::text FROM "tasks" t WHERE "id"=701`).Scan(&sourceAfter).Error)
				assert.JSONEq(t, before, sourceAfter, "archiving must not alter the original task or its credential compatibility")
				require.NoError(t, db.Raw(`SELECT jsonb_agg(to_jsonb(o) ORDER BY source_task_id)::text FROM "legacy_task_obligations" o`).Scan(&evidenceJSON).Error)
				assert.NotContains(t, evidenceJSON, "fixture-not-a-usable-key")
				assert.NotContains(t, evidenceJSON, "private_data")
				if pass == 0 {
					firstEvidence = evidenceJSON
				} else {
					assert.JSONEq(t, firstEvidence, evidenceJSON)
				}
				for _, table := range []string{"users", "providers", "tokens", "wallets", "ledger_entries", "gateway_billings", "subscription_periods"} {
					var count int64
					require.NoError(t, db.Table(table).Count(&count).Error)
					assert.Zero(t, count, "archived source references cannot create parents or financial facts: %s", table)
				}
			}
		})
	}
}

func TestLegacySQLiteIdentityKinds(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("requires the SQLite source reader")
	}
	db := model.UseTestDatabases(t)
	path := filepath.Join(t.TempDir(), "identity.sqlite")
	source := &legacySource{kind: "sqlite", path: path}
	command := exec.CommandContext(t.Context(), "sqlite3", path, `
 CREATE TABLE "implicit_identity" ("id" INTEGER, PRIMARY KEY("id"));
 INSERT INTO "implicit_identity" VALUES (7);
 CREATE TABLE "empty_identity" ("id" INTEGER PRIMARY KEY);
 CREATE TABLE "descending_key" ("id" INTEGER PRIMARY KEY DESC);
 CREATE TABLE "composite_key" ("id" INTEGER, "part" INTEGER, PRIMARY KEY("id","part"));
 CREATE TABLE "without_rowid" ("id" INTEGER PRIMARY KEY) WITHOUT ROWID;`)
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.NoError(t, db.Exec(`
 CREATE TABLE "implicit_identity" ("id" bigint PRIMARY KEY);
 INSERT INTO "implicit_identity" VALUES (7);
 CREATE TABLE "empty_identity" ("id" bigint PRIMARY KEY);
 CREATE TABLE "descending_key" ("id" bigint PRIMARY KEY);
 CREATE TABLE "composite_key" ("id" bigint, "part" bigint DEFAULT 1, PRIMARY KEY("id","part"));
 CREATE TABLE "without_rowid" ("id" bigint PRIMARY KEY);`).Error)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	require.NoError(t, restoreSourceIdentity(t.Context(), db, source, schema))
	for _, test := range []struct {
		table string
		want  int64
	}{{"implicit_identity", 8}, {"empty_identity", 1}} {
		var id int64
		require.NoError(t, db.Raw("INSERT INTO "+quoteIdentifier(test.table)+` DEFAULT VALUES RETURNING "id"`).Scan(&id).Error)
		assert.Equal(t, test.want, id, test.table)
	}
	for _, table := range []string{"descending_key", "composite_key", "without_rowid"} {
		assert.Error(t, db.Exec("INSERT INTO "+quoteIdentifier(table)+" DEFAULT VALUES").Error, "%s must still require an explicit source identity", table)
	}
}

func TestLegacySubscriptionMoneyUpgrade(t *testing.T) {
	dsn := testdb.PostgresDSN(t)
	require.NotEmpty(t, dsn)
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	previousDB, previousMaster := model.DB, common.IsMasterNode
	t.Cleanup(func() { model.DB, common.IsMasterNode = previousDB, previousMaster })
	common.IsMasterNode = true
	require.NoError(t, model.InitDB())
	db := model.DB
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	// Recreate the released column contract, including instance terms which differ
	// from the current plan. A canonical upgrade cannot grant the plan's new limit.
	require.NoError(t, db.Exec(`ALTER TABLE "subscription_plans"
	 DROP COLUMN IF EXISTS "price_nanos", DROP COLUMN IF EXISTS "price_currency",
	 DROP COLUMN IF EXISTS "allowance_nanos", DROP COLUMN IF EXISTS "allowance_currency",
	 DROP COLUMN IF EXISTS "unlimited", DROP COLUMN IF EXISTS "money_version",
	 ADD COLUMN IF NOT EXISTS "price_amount" numeric(10,6) NOT NULL, ADD COLUMN IF NOT EXISTS "currency" varchar(8) NOT NULL DEFAULT 'USD',
	 ADD COLUMN IF NOT EXISTS "total_amount" bigint NOT NULL DEFAULT 0;
	 ALTER TABLE "user_subscriptions"
	 DROP COLUMN IF EXISTS "allowance_nanos", DROP COLUMN IF EXISTS "allowance_currency",
	 DROP COLUMN IF EXISTS "unlimited", DROP COLUMN IF EXISTS "money_version",
	 DROP COLUMN IF EXISTS "current_period_id",
	 ADD COLUMN IF NOT EXISTS "amount_total" bigint NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS "amount_used" bigint NOT NULL DEFAULT 0;
	 INSERT INTO "subscription_plans" (id,title,price_amount,currency,total_amount,enabled,quota_reset_period)
	 VALUES (701,'Retained terms',17.123456,'EUR',750000,true,'monthly');
	 INSERT INTO "user_subscriptions" (id,user_id,plan_id,amount_total,amount_used,status,start_time,end_time,last_reset_time,next_reset_time,allow_wallet_overflow)
	 VALUES (701,701,701,500000,125000,'active',1700000000,2000000000,1701000000,2000000000,false),
	 (702,701,701,1000000,500000,'expired',1600000000,1700000000,1601000000,1700000000,true),
	 (703,701,701,500000,500000,'cancelled',1700000000,2000000000,1701000000,2000000000,false),
	 (704,701,701,0,250000,'active',1700000000,2000000000,1701000000,2000000000,true)`).Error)
	require.NoError(t, db.Create(&model.User{Id: 701, Username: "subscription-migration", Password: "fixture-hash", Status: 1, AffCode: "subscription-migration"}).Error)
	var before string
	require.NoError(t, db.Raw(`SELECT jsonb_agg(to_jsonb(s) ORDER BY id)::text FROM "user_subscriptions" s`).Scan(&before).Error)
	for range 2 {
		require.NoError(t, model.InitDB())
		connection, err := model.DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = connection.Close() })
	}
	require.True(t, db.Migrator().HasColumn("subscription_plans", "price_nanos"), "startup must convert released subscription prices to exact money")
	var plan struct {
		PriceNanos, AllowanceNanos       int64
		PriceCurrency, AllowanceCurrency string
		Unlimited                        bool
		MoneyVersion                     int
	}
	require.NoError(t, db.Table("subscription_plans").Where("id = ?", 701).Take(&plan).Error)
	assert.EqualValues(t, 17123456000, plan.PriceNanos)
	assert.Equal(t, "EUR", plan.PriceCurrency)
	assert.EqualValues(t, 1500000000, plan.AllowanceNanos)
	assert.Equal(t, "USD", plan.AllowanceCurrency)
	assert.False(t, plan.Unlimited)
	assert.Equal(t, 1, plan.MoneyVersion)
	var subscriptions []struct {
		ID                int
		AllowanceNanos    int64
		AllowanceCurrency string
		Unlimited         bool
		MoneyVersion      int
		CurrentPeriodID   int64
	}
	require.NoError(t, db.Table("user_subscriptions").Order("id").Find(&subscriptions).Error)
	require.Len(t, subscriptions, 4)
	for index, subscription := range subscriptions {
		assert.Equal(t, []int64{1000000000, 2000000000, 1000000000, 0}[index], subscription.AllowanceNanos)
		assert.Equal(t, "USD", subscription.AllowanceCurrency)
		assert.Equal(t, index == 3, subscription.Unlimited)
		assert.Equal(t, 1, subscription.MoneyVersion)
		assert.Positive(t, subscription.CurrentPeriodID)
		period, err := model.GetSubscriptionPeriodSummary(t.Context(), subscription.ID)
		require.NoError(t, err)
		assert.Equal(t, subscription.CurrentPeriodID, period.PeriodID)
		assert.EqualValues(t, 1, period.Sequence)
		assert.Equal(t, subscription.AllowanceNanos, period.Allowance.Nanos)
		assert.Equal(t, []int64{250000000, 1000000000, 1000000000, 500000000}[index], period.Consumed.Nanos)
		assert.Equal(t, []int64{750000000, 1000000000, 0, 0}[index], period.Available.Nanos)
		assert.Equal(t, index == 3, period.Unlimited)
		assert.Zero(t, period.Reserved.Nanos)
	}
	var retained string
	require.NoError(t, db.Raw(`SELECT jsonb_agg(to_jsonb(s) - ARRAY['allowance_nanos','allowance_currency','unlimited','money_version','current_period_id'] ORDER BY id)::text FROM "user_subscriptions" s`).Scan(&retained).Error)
	assert.JSONEq(t, before, retained, "upgrade must preserve source usage, state, reset times and overflow terms")
	var requestRevenue int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("account_type = ?", model.LedgerAccountPlatformRevenue).Count(&requestRevenue).Error)
	assert.Zero(t, requestRevenue, "historical usage is not new request revenue")
	for table, expected := range map[string]int64{"subscription_plan_money_openings": 1, "subscription_money_openings": 4, "subscription_periods": 4, "ledger_entries": 7} {
		var count int64
		require.NoError(t, db.Table(table).Count(&count).Error)
		assert.Equal(t, expected, count, table)
	}
	for _, statement := range []string{
		`UPDATE "subscription_plan_money_openings" SET "source_total"=0`,
		`DELETE FROM "subscription_money_openings"`,
		`TRUNCATE "subscription_money_openings"`,
	} {
		assert.Error(t, db.Exec(statement).Error, "opening evidence must be immutable")
	}

	t.Run("legacy preconsumes remain explicit unverified obligations", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		t.Cleanup(func() { _ = tx.Rollback().Error })
		require.NoError(t, tx.Exec(`INSERT INTO "subscription_pre_consume_records"
	 (id,request_id,user_id,user_subscription_id,pre_consumed,status,created_at,updated_at) VALUES
	 (901,'legacy-current-unknown',701,701,100000,'consumed',1701500000,1701500010),
	 (902,'legacy-refunded',701,702,1000,'refunded',1600500000,1600500010),
	 (903,'legacy-cancelled-unknown',701,703,1000,'consumed',1700500000,1700500010),
	 (904,'legacy-unlimited-unknown',701,704,0,'consumed',1700500000,1700500010)`).Error)
		var before string
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text FROM "ledger_entries" e`).Scan(&before).Error)
		for range 2 {
			require.NoError(t, model.MigrateSubscriptionMoney(tx))
		}
		require.True(t, tx.Migrator().HasTable("legacy_subscription_obligations"), "retaining old rows is not a recoverable reconciliation state")
		var obligations []struct {
			SourceRecordID                   int
			RequestID                        string
			UserID, SubscriptionID           int
			SourcePreConsumed                int64
			SourceStatus, State              string
			SourceCreatedAt, SourceUpdatedAt int64
		}
		require.NoError(t, tx.Table("legacy_subscription_obligations").Order("source_record_id").Find(&obligations).Error)
		require.Len(t, obligations, 4)
		for index, obligation := range obligations {
			assert.Equal(t, 901+index, obligation.SourceRecordID)
			assert.Equal(t, 701+index, obligation.SubscriptionID)
			assert.Equal(t, 701, obligation.UserID)
			assert.Equal(t, []string{"legacy-current-unknown", "legacy-refunded", "legacy-cancelled-unknown", "legacy-unlimited-unknown"}[index], obligation.RequestID)
			assert.Equal(t, []int64{100000, 1000, 1000, 0}[index], obligation.SourcePreConsumed)
			assert.Equal(t, []string{"consumed", "refunded", "consumed", "consumed"}[index], obligation.SourceStatus)
			assert.Equal(t, []string{"unverified", "source_refunded", "unverified", "unverified"}[index], obligation.State)
			assert.Equal(t, []int64{1701500000, 1600500000, 1700500000, 1700500000}[index], obligation.SourceCreatedAt)
			assert.Equal(t, obligation.SourceCreatedAt+10, obligation.SourceUpdatedAt)
		}
		var after string
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text FROM "ledger_entries" e`).Scan(&after).Error)
		assert.JSONEq(t, before, after, "unknown old preconsumes must not create refunds, new holds, revenue or new-period credits")
		var sourceCount int64
		require.NoError(t, tx.Model(&model.SubscriptionPreConsumeRecord{}).Count(&sourceCount).Error)
		assert.EqualValues(t, 4, sourceCount)
		pending, err := model.GetLegacySubscriptionReconciliationPending(tx, []int{701, 702, 703, 704, 999})
		require.NoError(t, err)
		assert.Equal(t, map[int]bool{701: true, 702: true, 703: true, 704: true}, pending, "historical aggregate usage remains unverified even where preconsume evidence was refunded or deleted")
		pending, err = model.GetLegacySubscriptionReconciliationPending(tx, []int{703})
		require.NoError(t, err)
		assert.Equal(t, map[int]bool{703: true}, pending, "decoration must not return subscriptions outside the authorized input")

		for _, statement := range []string{
			`UPDATE "legacy_subscription_obligations" SET "source_pre_consumed"=0`,
			`DELETE FROM "legacy_subscription_obligations"`,
			`TRUNCATE "legacy_subscription_obligations"`,
		} {
			err := tx.Transaction(func(child *gorm.DB) error { return child.Exec(statement).Error })
			assert.ErrorContains(t, err, "legacy_subscription_obligations is immutable")
		}
		for _, test := range []struct {
			name, update, expected string
		}{
			{"rebound instance", `"user_subscription_id"=702`, "changed after capture"},
			{"negative source", `"pre_consumed"=-1`, "invalid source identity or amount"},
			{"unknown owner", `"user_id"=999`, "invalid source identity or amount"},
		} {
			t.Run(test.name, func(t *testing.T) {
				require.NoError(t, tx.SavePoint("source_change").Error)
				require.NoError(t, tx.Exec(`UPDATE "subscription_pre_consume_records" SET `+test.update+` WHERE "id"=901`).Error)
				assert.ErrorContains(t, model.MigrateSubscriptionMoney(tx), test.expected)
				require.NoError(t, tx.RollbackTo("source_change").Error)
			})
		}
		require.NoError(t, tx.Exec(`INSERT INTO "subscription_pre_consume_records"
 (id,request_id,user_id,user_subscription_id,pre_consumed,status,created_at,updated_at)
 SELECT i, 'legacy-batch-' || i::text, 701, 701, 1, 'consumed', 1701500000, 1701500010
 FROM generate_series(1001,1251) i`).Error)
		for range 2 {
			require.NoError(t, model.MigrateSubscriptionMoney(tx))
		}
		var evidenceCount int64
		require.NoError(t, tx.Model(&model.LegacySubscriptionObligation{}).Count(&evidenceCount).Error)
		assert.EqualValues(t, 255, evidenceCount, "batch boundaries must neither drop nor duplicate obligations")
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text FROM "ledger_entries" e`).Scan(&after).Error)
		assert.JSONEq(t, before, after)
	})

	t.Run("deleted legacy instances retain independent preconsume evidence", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		t.Cleanup(func() { _ = tx.Rollback().Error })
		// Released AdminDeleteUserSubscription removed only the instance. Its
		// original preconsume facts remain valid evidence, not a new entitlement.
		require.NoError(t, tx.Exec(`INSERT INTO "subscription_pre_consume_records"
	 (id,request_id,user_id,user_subscription_id,pre_consumed,status,created_at,updated_at) VALUES
	 (910,'deleted-instance-consumed',701,799,1250,'consumed',1701500000,1701500010),
	 (911,'deleted-instance-refunded',701,799,250,'refunded',1701500000,1701500010)`).Error)
		var beforeLedger, beforeSource string
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text FROM "ledger_entries" e`).Scan(&beforeLedger).Error)
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM "subscription_pre_consume_records" p`).Scan(&beforeSource).Error)
		for range 2 {
			require.NoError(t, model.MigrateSubscriptionMoney(tx), "a legitimate historical deletion must not block startup")
		}
		var evidence []model.LegacySubscriptionObligation
		require.NoError(t, tx.Order("source_record_id").Find(&evidence).Error)
		require.Len(t, evidence, 2)
		for index, record := range evidence {
			assert.Equal(t, 910+index, record.SourceRecordID)
			assert.Equal(t, 701, record.UserID)
			assert.Equal(t, 799, record.SubscriptionID)
			assert.Equal(t, []string{"deleted-instance-consumed", "deleted-instance-refunded"}[index], record.RequestID)
			assert.Equal(t, []int64{1250, 250}[index], record.SourcePreConsumed)
			assert.Equal(t, []string{"consumed", "refunded"}[index], record.SourceStatus)
			assert.Equal(t, []string{"unverified", "source_refunded"}[index], record.State)
			assert.EqualValues(t, 1701500000, record.SourceCreatedAt)
			assert.EqualValues(t, 1701500010, record.SourceUpdatedAt)
		}
		var afterLedger, afterSource string
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(e) ORDER BY id)::text FROM "ledger_entries" e`).Scan(&afterLedger).Error)
		require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM "subscription_pre_consume_records" p`).Scan(&afterSource).Error)
		assert.JSONEq(t, beforeLedger, afterLedger, "orphan evidence cannot create money, refunds or holds")
		assert.JSONEq(t, beforeSource, afterSource)
		for _, table := range []string{"user_subscriptions", "subscription_money_openings", "subscription_periods"} {
			var count int64
			require.NoError(t, tx.Table(table).Count(&count).Error)
			assert.EqualValues(t, 4, count, "orphan evidence cannot fabricate an instance or period: %s", table)
		}
		// Missing parent is not permission to accept an existing foreign owner.
		require.NoError(t, tx.Exec(`INSERT INTO "users" (id,username,password,status,aff_code) VALUES (702,'other-evidence-owner','fixture-hash',1,'other-evidence-owner');
	 INSERT INTO "subscription_pre_consume_records" (id,request_id,user_id,user_subscription_id,pre_consumed,status)
	 VALUES (912,'conflicting-existing-owner',702,701,1,'consumed')`).Error)
		assert.ErrorContains(t, model.MigrateSubscriptionMoney(tx), "invalid source identity or amount")
		var count int64
		require.NoError(t, tx.Model(&model.LegacySubscriptionObligation{}).Where("source_record_id = ?", 912).Count(&count).Error)
		assert.Zero(t, count)
	})

	t.Run("accepted payment contracts cannot be rewritten", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		t.Cleanup(func() { _ = tx.Rollback().Error })
		contract := model.SubscriptionOrderMoney{
			TradeNo:       "accepted-subscription-contract",
			ContractPrice: money.Amount{Nanos: 9007199254740993, Currency: "USD"},
			WalletCharge:  money.Amount{Currency: "USD"},
		}
		require.NoError(t, tx.Create(&contract).Error)
		for _, mutation := range []string{
			`"contract_price_nanos"=1`,
			`"contract_price_currency"='EUR'`,
			`"wallet_charge_nanos"=1`,
			`"wallet_charge_currency"='EUR'`,
			`"fx_rate"=2`,
			`"grant_terms_version"=1`,
			`"grant_terms"='{"unlimited":true}'`,
			`"trade_no"='changed-contract'`,
			`"user_subscription_id"=-1`,
		} {
			err := tx.Transaction(func(child *gorm.DB) error {
				return child.Exec(`UPDATE "subscription_order_moneys" SET ` + mutation).Error
			})
			assert.ErrorContains(t, err, "subscription payment contract is immutable")
		}
		require.NoError(t, tx.Model(&model.SubscriptionOrderMoney{}).Where("trade_no = ?", contract.TradeNo).Update("user_subscription_id", 701).Error)
		require.NoError(t, tx.Model(&model.SubscriptionOrderMoney{}).Where("trade_no = ?", contract.TradeNo).Update("user_subscription_id", 701).Error, "same granted identity is an idempotent write")
		for _, statement := range []string{
			`UPDATE "subscription_order_moneys" SET "user_subscription_id"=702`,
			`UPDATE "subscription_order_moneys" SET "user_subscription_id"=0`,
			`DELETE FROM "subscription_order_moneys"`,
			`TRUNCATE "subscription_order_moneys"`,
		} {
			err := tx.Transaction(func(child *gorm.DB) error { return child.Exec(statement).Error })
			assert.ErrorContains(t, err, "subscription payment contract is immutable")
		}
		var stored model.SubscriptionOrderMoney
		require.NoError(t, tx.First(&stored, "trade_no = ?", contract.TradeNo).Error)
		assert.Equal(t, contract.ContractPrice, stored.ContractPrice)
		assert.Equal(t, contract.WalletCharge, stored.WalletCharge)
		assert.False(t, stored.FXRate.Valid)
		assert.Zero(t, stored.GrantTermsVersion)
		assert.Empty(t, stored.GrantTerms)
		assert.Equal(t, 701, stored.UserSubscriptionID)
	})

	t.Run("new records do not require or fabricate legacy money", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		t.Cleanup(func() { _ = tx.Rollback().Error })
		plan := model.SubscriptionPlan{Title: "Canonical after upgrade", Price: money.Amount{Nanos: 1000000001, Currency: "USD"}, Allowance: money.Amount{Nanos: 2000000000, Currency: "USD"}, MoneyVersion: 1}
		require.NoError(t, tx.Create(&plan).Error, "retired NOT NULL source fields must not force new floating-point or quota writes")
		var absent bool
		require.NoError(t, tx.Raw(`SELECT "price_amount" IS NULL AND "currency" IS NULL AND "total_amount" IS NULL FROM "subscription_plans" WHERE "id"=?`, plan.Id).Scan(&absent).Error)
		assert.True(t, absent)
		subscription := model.UserSubscription{UserId: 701, PlanId: plan.Id, Allowance: plan.Allowance, MoneyVersion: 1, Status: "active"}
		require.NoError(t, tx.Create(&subscription).Error)
		require.NoError(t, tx.Raw(`SELECT "amount_total" IS NULL AND "amount_used" IS NULL FROM "user_subscriptions" WHERE "id"=?`, subscription.Id).Scan(&absent).Error)
		assert.True(t, absent)
	})

	t.Run("previously completed gateway still converts subscriptions", func(t *testing.T) {
		tx := db.Begin()
		require.NoError(t, tx.Error)
		previous := model.DB
		model.DB = tx
		t.Cleanup(func() { model.DB = previous; _ = tx.Rollback().Error })
		declareLegacyAbilityFixture(t, tx)
		require.NoError(t, tx.Exec(`CREATE TABLE IF NOT EXISTS "legacy_gateway_migrations" ("version" varchar(64) PRIMARY KEY,"completed_at" bigint NOT NULL);
 INSERT INTO "legacy_gateway_migrations" VALUES ('legacy_gateway_v1',1),('legacy_midjourney_obligations_v2',1);
 ALTER TABLE "subscription_plans" ALTER COLUMN "price_amount" TYPE numeric;
 INSERT INTO "subscription_plans" (id,title,price_amount,currency,total_amount,price_nanos,price_currency,allowance_nanos,allowance_currency,money_version)
 VALUES (801,'Previously unconverted',9007199.254740993,'USD',500000,0,'USD',0,'USD',0),
 (802,'Receivable rounding',0.0000000001,'USD',0,0,'USD',0,'USD',0);
 INSERT INTO "user_subscriptions" (id,user_id,plan_id,amount_total,amount_used,allowance_nanos,allowance_currency,money_version)
 VALUES (801,701,801,500000,1,0,'USD',0)`).Error)
		for pass := range 2 {
			report, err := model.MigrateLegacyGateway(t.Context())
			require.NoError(t, err)
			assert.True(t, report.Committed)
			assert.Equal(t, pass > 0, report.AlreadyCompleted, "the first pass independently retires the old routing source")
			assert.False(t, tx.Migrator().HasTable("abilities"))
		}
		var exact model.SubscriptionPlan
		require.NoError(t, tx.First(&exact, 801).Error)
		assert.EqualValues(t, 9007199254740993, exact.Price.Nanos)
		var rounded model.SubscriptionPlan
		require.NoError(t, tx.First(&rounded, 802).Error)
		assert.EqualValues(t, 1, rounded.Price.Nanos)
		assert.True(t, rounded.Unlimited)
		period, err := model.GetSubscriptionPeriodSummary(t.Context(), 801)
		require.NoError(t, err)
		assert.EqualValues(t, 1, period.Sequence)
		assert.EqualValues(t, 2000, period.Consumed.Nanos)
		assert.EqualValues(t, 999998000, period.Available.Nanos)
		var periodCount int64
		require.NoError(t, tx.Model(&model.SubscriptionPeriod{}).Count(&periodCount).Error)
		assert.EqualValues(t, 5, periodCount)
	})

	for _, test := range []struct {
		name, price, currency string
		total, used           int64
	}{
		{name: "negative price", price: "-0.000000001", currency: "USD", total: 500000},
		{name: "invalid price", price: "NaN", currency: "USD", total: 500000},
		{name: "price overflow", price: "9223372036.854775808", currency: "USD", total: 500000},
		{name: "invalid currency", price: "1", currency: "usd", total: 500000},
		{name: "negative allowance", price: "1", currency: "USD", total: -1},
		{name: "negative consumed", price: "1", currency: "USD", total: 500000, used: -1},
		{name: "consumed beyond finite allowance", price: "1", currency: "USD", total: 500000, used: 500001},
		{name: "allowance overflow", price: "1", currency: "USD", total: math.MaxInt64},
		{name: "unlimited consumed overflow", price: "1", currency: "USD", total: 0, used: math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			t.Cleanup(func() { _ = tx.Rollback().Error })
			// One earlier valid plan must not remain converted when a later source
			// is invalid. Numeric source text is never narrowed before validation.
			require.NoError(t, tx.Exec(`ALTER TABLE "subscription_plans" ALTER COLUMN "price_amount" TYPE numeric;
 INSERT INTO "subscription_plans" (id,title,price_amount,currency,total_amount,price_nanos,price_currency,allowance_nanos,allowance_currency,money_version)
 VALUES (801,'Earlier valid source',1,'USD',500000,0,'USD',0,'USD',0)`).Error)
			require.NoError(t, tx.Exec(`INSERT INTO "subscription_plans" (id,title,price_amount,currency,total_amount,price_nanos,price_currency,allowance_nanos,allowance_currency,money_version)
 VALUES (802,'Invalid source',?::numeric,?,500000,0,'USD',0,'USD',0)`, test.price, test.currency).Error)
			require.NoError(t, tx.Exec(`INSERT INTO "user_subscriptions" (id,user_id,plan_id,amount_total,amount_used,allowance_nanos,allowance_currency,money_version)
 VALUES (802,701,802,?,?,0,'USD',0)`, test.total, test.used).Error)
			var sourceBefore string
			require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM "subscription_plans" p WHERE id>=801`).Scan(&sourceBefore).Error)
			require.Error(t, model.MigrateSubscriptionMoney(tx))
			var sourceAfter string
			require.NoError(t, tx.Raw(`SELECT jsonb_agg(to_jsonb(p) ORDER BY id)::text FROM "subscription_plans" p WHERE id>=801`).Scan(&sourceAfter).Error)
			assert.JSONEq(t, sourceBefore, sourceAfter)
			for table, expected := range map[string]int64{"subscription_plan_money_openings": 1, "subscription_money_openings": 4, "subscription_periods": 4, "ledger_entries": 7} {
				var count int64
				require.NoError(t, tx.Table(table).Count(&count).Error)
				assert.Equal(t, expected, count, "failed conversion must roll back %s", table)
			}
		})
	}
}

func TestLegacyPricingConversionSourceSnapshot(t *testing.T) {
	dsn := testdb.PostgresDSN(t)
	require.NotEmpty(t, dsn)
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	previousDB, previousMaster, previousGeneration, previousQuota := model.DB, common.IsMasterNode, jsplugin.DefaultRegistry, common.QuotaPerUnit
	t.Cleanup(func() {
		model.DB, common.IsMasterNode, jsplugin.DefaultRegistry, common.QuotaPerUnit = previousDB, previousMaster, previousGeneration, previousQuota
	})
	common.IsMasterNode, common.QuotaPerUnit = true, 500000
	require.NoError(t, model.InitDB())
	db := model.DB.Session(&gorm.Session{Logger: logger.Discard})
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	declareLegacyAbilityFixture(t, db)
	require.NoError(t, db.Exec(`INSERT INTO providers (id,"key",type,status,name,models,model_mapping,setting) VALUES
	 (81,'',1,1,'source-text','source-text','{}','{}'),
	 (82,'',55,1,'source-video','source-video','{}','{}'),
	 (83,'',1,1,'source-cycle','source-cycle','{"source-cycle":"cycle-tail","cycle-tail":"source-cycle"}','{}'),
	 (84,'',60,1,'source-task','source-task','{"source-task":"mj_imagine"}','{"task_extend_plugin_keys":["midjourney"]}');
	 INSERT INTO abilities ("group",model,channel_id,enabled) VALUES
	 ('default','source-text',81,true),('default','source-video',82,false),
	 ('default','source-cycle',83,false),('default','source-task',84,true)`).Error)
	var captured *model.LegacyPricingConversionSource
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		var err error
		captured, err = model.LoadLegacyPricingConversionSource(tx, jsplugin.DefaultRegistry.Generation())
		return err
	}))
	// Mutating the source tables or the published plugin generation afterwards
	// cannot change the captured conversion, and there is no global DB to read.
	require.NoError(t, db.Exec(`DELETE FROM abilities; UPDATE providers SET type=1,model_mapping='{}',setting='{}'`).Error)
	model.DB, jsplugin.DefaultRegistry = nil, jsplugin.NewRegistry()
	ordinary, err := captured.Preview("source-text", model.PricingValues{"ModelRatio": 0.125, "CompletionRatio": 2.0})
	require.NoError(t, err)
	assert.Equal(t, `tier("base", p * 0.25 + c * 0.5)`, ordinary.Expression)
	for _, name := range []string{"source-video", "source-cycle", "source-task"} {
		conversion, err := captured.Preview(name, model.PricingValues{"ModelPrice": 1.0})
		require.NoError(t, err)
		assert.NotEmpty(t, conversion.UnsupportedReason, name)
		assert.Empty(t, conversion.Expression, name)
	}
	missing, err := captured.Preview("unconfigured-source", model.PricingValues{})
	require.NoError(t, err)
	assert.NotEmpty(t, missing.UnsupportedReason)
	zero, err := captured.Preview("unconfigured-source", model.PricingValues{"ModelPrice": 0.0})
	require.NoError(t, err)
	assert.Equal(t, `tier("request", fixed(0))`, zero.Expression)
	var absent *model.LegacyPricingConversionSource
	_, err = absent.Preview("source-text", model.PricingValues{})
	require.Error(t, err)
	_, err = model.LoadLegacyPricingConversionSource(nil, previousGeneration.Generation())
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = model.LoadLegacyPricingConversionSource(db.WithContext(ctx), previousGeneration.Generation())
	require.ErrorIs(t, err, context.Canceled)

	// A source read failure must roll back the migration and its completion
	// marker, rather than turn a priced text model into a disabled success.
	model.DB, jsplugin.DefaultRegistry = db, previousGeneration
	require.NoError(t, db.Create(&model.User{Id: 81, Username: "source-root", Password: "fixture-hash", Role: 100, Status: 1, AffCode: "source-root"}).Error)
	require.NoError(t, db.Exec(`INSERT INTO options (key,value) VALUES ('ModelRatio','{"source-text":0.125}')`).Error)
	injected := errors.New("injected legacy source read failure")
	callback := "test:legacy_source_read_failure"
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "abilities" {
			tx.AddError(injected)
		}
	}))
	report, err := model.MigrateLegacyGateway(context.Background())
	require.NoError(t, db.Callback().Row().Remove(callback))
	require.ErrorIs(t, err, model.ErrLegacyGatewayMigrationFailed)
	assert.False(t, report.Committed)
	var count int64
	require.NoError(t, db.Model(&model.ModelOffering{}).Count(&count).Error)
	assert.Zero(t, count)
	assert.False(t, db.Migrator().HasTable("legacy_gateway_migrations"))
}

func TestLegacyMigrationTaskPricingSource(t *testing.T) {
	for _, test := range []struct {
		name, model, settings, override, expression string
		mapping, overrideModel                      string
		providerType                                int
		persisted, mappedExpression                 bool
		nanos                                       int64
	}{
		{name: "plugin_override", model: "sora-2", providerType: 1, settings: `{}`, override: "0.2", expression: "0.2", nanos: 200000000},
		{name: "explicit_zero_override", model: "sora-2", providerType: 1, settings: `{}`, override: "0", expression: "0", nanos: 0},
		{name: "client_override_before_mapped", model: "source-alias", mapping: "sora-2", providerType: 1, settings: `{}`, override: "0.2", expression: "0.2", nanos: 200000000},
		{name: "mapped_override_before_client_expression", model: "source-alias", mapping: "sora-2", overrideModel: "sora-2", providerType: 1, settings: `{}`, override: "0.2", expression: "0.2", nanos: 200000000},
		{name: "mapped_model_expression", model: "source-alias", mapping: "sora-2", mappedExpression: true, providerType: 1, settings: `{}`, expression: "0.7", nanos: 700000000},
		{name: "type60_extension", model: "sora-2", providerType: 60, settings: `{"task_extend_plugin_keys":["sora"]}`, expression: "0.7", nanos: 700000000},
		{name: "type61_binding", model: "sora-2", providerType: 61, settings: `{"task_plugin_key":"sora"}`, expression: "0.7", nanos: 700000000},
		{name: "persisted_active_source", model: "audit-custom-model", providerType: 1, settings: `{}`, persisted: true, expression: "0.7", nanos: 700000000},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := testdb.PostgresDSN(t)
			require.NotEmpty(t, dsn)
			t.Setenv("SQL_DSN", dsn)
			t.Setenv("SETTLEMENT_CURRENCY", "USD")
			previousDB, previousMaster, previousQuota, previousRegistry := model.DB, common.IsMasterNode, common.QuotaPerUnit, jsplugin.DefaultRegistry
			t.Cleanup(func() {
				model.DB, common.IsMasterNode, common.QuotaPerUnit, jsplugin.DefaultRegistry = previousDB, previousMaster, previousQuota, previousRegistry
			})
			common.IsMasterNode, common.QuotaPerUnit = true, 500000
			require.NoError(t, model.InitDB())
			db := model.DB.Session(&gorm.Session{Logger: logger.Discard})
			model.DB = db
			pool, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			declareLegacyAbilityFixture(t, db)
			if test.persisted {
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				source = strings.Replace(source, `models: ["sora-2", "sora-2-pro"]`, `models: ["audit-custom-model"]`, 1)
				source = strings.Replace(source, `version: "1.2.0"`, `version: "9.9.9"`, 1)
				compiled, err := jsplugin.CompilePlugin(source, jsplugin.Options{Key: "sora", Version: "9.9.9"})
				require.NoError(t, err)
				require.Equal(t, []string{"audit-custom-model"}, compiled.Meta.Models)
				hash := sha256.Sum256([]byte(source))
				require.NoError(t, model.SaveTaskPlugin(&model.TaskPlugin{Key: "sora", APIVersion: 1, Version: "9.9.9", Source: model.LongText(source), SourceHash: fmt.Sprintf("%x", hash), Enabled: true}))
			}
			require.NoError(t, db.Create(&model.User{Id: 901, Username: "source-task-root", Password: "fixture-hash", Role: 100, Status: 1, AffCode: "source-task-root"}).Error)
			mapping := map[string]string{}
			if test.mapping != "" {
				mapping[test.model] = test.mapping
			}
			mappingJSON, err := common.Marshal(mapping)
			require.NoError(t, err)
			require.NoError(t, db.Exec(`INSERT INTO providers (id,"key",type,status,name,models,"group",model_mapping,setting) VALUES (901,'',?,1,'source-task-provider',?,'default',?,?)`, test.providerType, test.model, string(mappingJSON), test.settings).Error)
			require.NoError(t, db.Exec(`INSERT INTO abilities ("group",model,channel_id,enabled) VALUES ('default',?,901,true)`, test.model).Error)
			expressionModel := test.model
			if test.mappedExpression {
				expressionModel = test.mapping
			}
			overrides := map[string]string{}
			if test.override != "" {
				overrideModel := test.model
				if test.mapping != "" {
					overrides["sora::"+test.mapping] = "0.4"
				}
				if test.overrideModel != "" {
					overrideModel = test.overrideModel
				}
				overrides["sora::"+overrideModel] = test.override
			}
			for key, value := range map[string]any{
				"billing_setting.billing_mode":          map[string]string{expressionModel: "tiered_expr"},
				"billing_setting.billing_expr":          map[string]string{expressionModel: "0.7"},
				billing_setting.PluginBillingExprOption: overrides,
			} {
				encoded, err := common.Marshal(value)
				require.NoError(t, err)
				require.NoError(t, db.Create(&model.Option{Key: key, Value: string(encoded)}).Error)
			}
			// The process generation deliberately has no plugins. Source migration
			// must neither depend on it nor publish into it.
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			runtimeGeneration := jsplugin.DefaultRegistry.Generation()
			report, err := model.MigrateLegacyGateway(context.Background())
			require.NoError(t, err)
			require.True(t, report.Committed)
			assert.Same(t, runtimeGeneration, jsplugin.DefaultRegistry.Generation())
			var channel model.Channel
			require.NoError(t, db.Where("provider_id = ?", 901).First(&channel).Error)
			assert.Equal(t, "task:sora", string(channel.Protocol))
			var offering model.ModelOffering
			require.NoError(t, db.Where("name = ?", test.model).First(&offering).Error)
			assert.True(t, offering.Enabled)
			var prices []model.PriceVersion
			require.NoError(t, db.Where("owner_type = ? AND owner_id = ?", model.PriceOwnerOffering, offering.ID).Find(&prices).Error)
			require.Len(t, prices, 1)
			assert.Equal(t, test.expression, prices[0].Expression)
			_, task := channel.Protocol.TaskPluginKey()
			amount, _, err := billingexpr.RunMoneyExpr(prices[0].Expression, billingexpr.MoneyInput{TaskUsage: task}, money.Currency("USD"))
			require.NoError(t, err)
			stored, err := amount.Receivable()
			require.NoError(t, err)
			assert.Equal(t, test.nanos, stored.Nanos, "the migrated protocol must preserve the task USD unit")
			assert.Equal(t, int64(1), report.Counts["sale_prices_created"])
			assert.Equal(t, int64(1), report.Counts["offerings_enabled"])
			repeat, err := model.MigrateLegacyGateway(context.Background())
			require.NoError(t, err)
			assert.True(t, repeat.AlreadyCompleted)
			assert.Zero(t, repeat.Counts["sale_prices_created"])
			var after []model.PriceVersion
			require.NoError(t, db.Where("owner_type = ? AND owner_id = ?", model.PriceOwnerOffering, offering.ID).Find(&after).Error)
			assert.Equal(t, prices, after)
			assert.Same(t, runtimeGeneration, jsplugin.DefaultRegistry.Generation())
			_, err = model.MutateChannel(context.Background(), channel.ID, model.ChannelSave, nil)
			require.EqualError(t, err, "Task plugin is not available", "ordinary mutation must still use the process generation")
			err = model.CreatePriceVersion(context.Background(), &model.PriceVersion{OwnerType: model.PriceOwnerOffering, OwnerID: offering.ID, Currency: "USD", Expression: test.expression, EffectiveAt: prices[0].EffectiveAt + 1}, 901)
			require.EqualError(t, err, "Task price requires an available plugin usage schema", "the private migration generation must not leak into ordinary price validation")
			var final []model.PriceVersion
			require.NoError(t, db.Where("owner_type = ? AND owner_id = ?", model.PriceOwnerOffering, offering.ID).Find(&final).Error)
			assert.Equal(t, prices, final)
		})
	}
}

func TestLegacyMigrationTaskSourceRefusesUnsafeFacts(t *testing.T) {
	for _, name := range []string{"factory_disabled", "plugins_disabled", "active_override_disabled", "active_source_invalid", "active_versions_ambiguous", "host_path_key", "route_prices_disagree", "selected_override_invalid"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SQL_DSN", testdb.PostgresDSN(t))
			t.Setenv("SETTLEMENT_CURRENCY", "USD")
			previousDB, previousMaster, previousRegistry := model.DB, common.IsMasterNode, jsplugin.DefaultRegistry
			t.Cleanup(func() {
				model.DB, common.IsMasterNode, jsplugin.DefaultRegistry = previousDB, previousMaster, previousRegistry
			})
			common.IsMasterNode = true
			require.NoError(t, model.InitDB())
			db := model.DB.Session(&gorm.Session{Logger: logger.Discard})
			model.DB = db
			pool, err := db.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = pool.Close() })
			declareLegacyAbilityFixture(t, db)
			modelName, mapping := "sora-2", "{}"
			if strings.HasPrefix(name, "active_") || name == "host_path_key" {
				modelName = "audit-custom-model"
				source, err := plugins.Source("sora")
				require.NoError(t, err)
				source = strings.Replace(source, `models: ["sora-2", "sora-2-pro"]`, `models: ["audit-custom-model"]`, 1)
				source = strings.Replace(source, `version: "1.2.0"`, `version: "9.9.9"`, 1)
				pluginKey := "sora"
				if name == "host_path_key" {
					pluginKey = "pg"
					source = strings.Replace(source, `key: "sora"`, `key: "pg"`, 1)
					source = strings.Replace(source, `channelTypes: [55, 1]`, `channelTypes: []`, 1)
				}
				_, err = jsplugin.CompilePlugin(source, jsplugin.Options{Key: pluginKey, Version: "9.9.9"})
				require.NoError(t, err, "the source is valid plugin code before host admission")
				hash := sha256.Sum256([]byte(source))
				row := model.TaskPlugin{Key: pluginKey, APIVersion: 1, Version: "9.9.9", Source: model.LongText(source), SourceHash: fmt.Sprintf("%x", hash), Enabled: name != "active_override_disabled", Active: true}
				if name == "active_source_invalid" {
					row.SourceHash = strings.Repeat("0", 64)
				}
				require.NoError(t, db.Create(&row).Error)
				if name == "active_versions_ambiguous" {
					row.Id, row.Version = 0, "9.9.8"
					require.NoError(t, db.Create(&row).Error)
				}
			}
			if name == "route_prices_disagree" {
				modelName, mapping = "source-alias", `{"source-alias":"sora-2"}`
				require.NoError(t, db.Exec(`INSERT INTO providers (id,"key",type,status,name,models,"group",model_mapping,setting) VALUES (902,'',1,1,'second-task','source-alias','default','{"source-alias":"sora-2-pro"}','{}')`).Error)
			}
			require.NoError(t, db.Create(&model.User{Id: 901, Username: "source-refusal-root", Password: "fixture-hash", Role: 100, Status: 1, AffCode: "source-refusal-root"}).Error)
			require.NoError(t, db.Exec(`INSERT INTO providers (id,"key",type,status,name,models,"group",model_mapping,setting) VALUES (901,'',1,1,'source-refusal',?,'default',?,'{}')`, modelName, mapping).Error)
			if name == "host_path_key" {
				require.NoError(t, db.Exec(`UPDATE providers SET type=61, setting='{"task_plugin_key":"pg"}' WHERE id=901`).Error)
			}
			require.NoError(t, db.Exec(`INSERT INTO abilities ("group",model,channel_id,enabled) VALUES ('default',?,901,false)`, modelName).Error)
			options := map[string]any{
				"billing_setting.billing_mode": map[string]string{modelName: "tiered_expr"},
				"billing_setting.billing_expr": map[string]string{modelName: "0.7"},
			}
			switch name {
			case "factory_disabled":
				options["TaskPluginDisabledFactoryKeys"] = []string{"sora"}
			case "plugins_disabled":
				require.NoError(t, db.Create(&model.Option{Key: "TaskPluginEnabled", Value: "false"}).Error)
			case "route_prices_disagree":
				options[billing_setting.PluginBillingExprOption] = map[string]string{"sora::sora-2": "0.2", "sora::sora-2-pro": "0.4"}
			case "selected_override_invalid":
				options[billing_setting.PluginBillingExprOption] = map[string]string{"sora::sora-2": "not_valid_task_usage"}
			}
			for key, value := range options {
				encoded, err := common.Marshal(value)
				require.NoError(t, err)
				require.NoError(t, db.Create(&model.Option{Key: key, Value: string(encoded)}).Error)
			}
			jsplugin.DefaultRegistry = jsplugin.NewRegistry()
			generation := jsplugin.DefaultRegistry.Generation()
			report, err := model.MigrateLegacyGateway(context.Background())
			assert.Same(t, generation, jsplugin.DefaultRegistry.Generation())
			if name == "route_prices_disagree" || name == "selected_override_invalid" {
				require.NoError(t, err)
				assert.True(t, report.Committed)
				assert.Zero(t, report.Counts["sale_prices_created"])
				assert.Zero(t, report.Counts["offerings_enabled"])
				assert.EqualValues(t, 1, report.Counts["offerings_disabled"])
				repeat, err := model.MigrateLegacyGateway(context.Background())
				require.NoError(t, err)
				assert.True(t, repeat.AlreadyCompleted)
			} else {
				require.Error(t, err)
				assert.False(t, report.Committed)
				assert.False(t, db.Migrator().HasTable("legacy_gateway_migrations"), "refusal must not leave a completion marker")
			}
			for _, table := range []string{"price_versions", "wallets", "ledger_entries"} {
				var count int64
				require.NoError(t, db.Table(table).Count(&count).Error)
				assert.Zero(t, count, table)
			}
		})
	}
}

func testLegacyMigrationReleasedDatabase(t *testing.T, kind, released string) {
	t.Helper()
	previousRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() { common.RedisEnabled = previousRedis })
	clients := []string{"pgloader", "pg_dump", "sqlite3"}
	if kind == "mysql" {
		clients = []string{"pgloader", "pg_dump", "mysql", "mysqldump"}
	}
	for _, client := range clients {
		_, err := exec.LookPath(client)
		require.NoError(t, err)
	}
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	require.NotEmpty(t, dsn, "requires an invocation-owned real PostgreSQL database")
	t.Setenv("LEGACY_MIGRATION_TEST_TARGET", dsn)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	common.DebugEnabled = false
	db, _, err := model.OpenDSN("TEST_POSTGRES_DSN", dsn, false)
	require.NoError(t, err)
	db = db.Session(&gorm.Session{Logger: logger.Discard})
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	var initialTables int64
	require.NoError(t, db.Raw("SELECT count(*) FROM pg_tables WHERE schemaname='public'").Scan(&initialTables).Error)
	require.Zero(t, initialTables, "test target must start empty")
	directory := t.TempDir()
	source := filepath.Join(directory, "legacy.sqlite")
	if kind == "sqlite" {
		data, err := os.ReadFile(released)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(source, data, 0o600))
	}
	inventory, err := prepareSource(kind, source, released)
	require.NoError(t, err)
	fixture := `
INSERT INTO users (id,username,password,role,status,quota,"group",aff_code,auth_version) VALUES
 (41,'migration-root','fixture-password-hash',100,1,-1,'default','migration-root-aff',1),
 (42,'migration-zero','fixture-password-hash',1,1,0,'vip','migration-zero-aff',1);
UPDATE users SET setting='{"quota_warning_enabled":false,"quota_warning_threshold":0.0001,"language":"fr"}' WHERE id=41;
UPDATE users SET setting='{"quota_warning_threshold":0,"language":"en"}' WHERE id=42;
DELETE FROM options WHERE "key" IN ('QuotaRemindThreshold','QuotaRemindEnabled');
INSERT INTO options ("key",value) VALUES ('QuotaRemindThreshold','1000'),('QuotaRemindEnabled','true');
INSERT INTO subscription_plans (id,title,price_amount,currency,total_amount,enabled,quota_reset_period) VALUES
 (701,'Released exact terms',17.123456,'EUR',750000,1,'monthly');
INSERT INTO user_subscriptions (id,user_id,plan_id,amount_total,amount_used,status,start_time,end_time,last_reset_time,next_reset_time,allow_wallet_overflow) VALUES
 (701,41,701,500000,125000,'active',1700000000,2000000000,1701000000,2000000000,0),
 (702,41,701,1000000,500000,'expired',1600000000,1700000000,1601000000,1700000000,1),
 (703,42,701,500000,500000,'cancelled',1700000000,2000000000,1701000000,2000000000,0),
 (704,42,701,0,250000,'active',1700000000,2000000000,1701000000,2000000000,1);
INSERT INTO subscription_pre_consume_records (id,request_id,user_id,user_subscription_id,pre_consumed,status,created_at,updated_at) VALUES
 (901,'released-subscription-request',41,701,100000,'consumed',1701500000,1701500010),
 (902,'released-subscription-refunded',41,702,1000,'refunded',1600500000,1600500010),
 (903,'released-subscription-cancelled',42,703,1000,'consumed',1700500000,1700500010),
 (904,'released-subscription-deleted-instance',41,799,1250,'consumed',1701500000,1701500010);
INSERT INTO tasks (id,task_id,platform,user_id,channel_id,quota,status,created_at,updated_at,private_data) VALUES
 (101,'released-subscription-task','sora',41,51,100000,'IN_PROGRESS',1701500000,1701500010,'{"billing_source":"subscription","subscription_id":701,"execution":{"request_id":"released-subscription-request"},"key":"fixture-retained-task-key"}'),
 (102,'released-subscription-cleaned-evidence','sora',42,51,0,'SUCCESS',1700500000,1700500010,'{"billing_source":"subscription","subscription_id":704}'),
 (103,'released-subscription-failed','sora',42,51,1000,'FAILURE',1700500000,1700500010,'{"billing_source":"subscription","subscription_id":703}'),
 (111,'released-wallet-task','sora',41,51,2500,'IN_PROGRESS',1701500000,1701500010,'{"billing_source":"wallet","token_id":61,"upstream_task_id":"released-private-task","execution":{"request_id":"released-wallet-request","task_plugin":{"key":"sora","version":"source-version","generation":18446744073709551615}},"key":"fixture-ordinary-task-key"}'),
 (112,'released-empty-source-task','sora',42,51,0,'SUCCESS',1700500000,1700500010,'{}'),
 (113,'released-unknown-source-task','sora',987,988,1250,'FAILURE',1700500000,1700500010,'{"billing_source":"unknown-source","token_id":989,"subscription_id":990}'),
 (114,'','sora',0,0,0,'IN_PROGRESS',1700500000,1700500010,NULL);
INSERT INTO channels (id,type,"key",status,name,models,"group",model_mapping,param_override) VALUES
 (51,1,'fixture-provider-one',1,'migration-provider-one','migration-text,unconvertible,explicit-zero,sora-2,opaque-sora-alias,sora-similar-undeclared','default','{"migration-text":"upstream-text-v2","opaque-sora-alias":"SORA-2"}','{"temperature":0}'),
 (52,1,'fixture-provider-two',1,'migration-provider-two','migration-text','vip','{"migration-alias":"migration-text"}','{}'),
 (53,2,'fixture-provider-mj',1,'migration-provider-mj','','default','{}','{}');
INSERT INTO channels (id,type,"key",status,name,models,"group",model_mapping,setting,param_override) VALUES
 (54,55,'fixture-video',1,'migration-video','opaque-video','default','{}','{}','{}'),
 (55,20,'fixture-router',1,'migration-router','claude-3-5-sonnet-20240620','default','{}','{}','{}'),
 (56,61,'fixture-task',1,'migration-task','opaque-task','default','{"opaque-task":"mj_imagine"}','{"task_plugin_key":"midjourney"}','{}'),
 (57,1,'fixture-zero',1,'migration-zero-price','explicit-zero','default','{}','{}','{}'),
 (58,1,'fixture-override',1,'migration-override','admin-expression','default','{}','{}','{}'),
 (59,1,'fixture-missing',1,'migration-missing','missing-price','default','{}','{}','{}'),
 (60,1,'fixture-alias',1,'migration-task-alias','opaque-task-alias','default','{"opaque-task-alias":"mj_imagine"}','{}','{}'),
 (61,1,'fixture-source',1,'migration-source-override','audit-custom-model','default','{}','{}','{}'),
 (62,60,'fixture-extension',1,'migration-extension','extension-sora','default','{"extension-sora":"sora-2-pro"}','{"task_extend_plugin_keys":["sora"]}','{}'),
 (63,1,'fixture-task-zero',1,'migration-task-zero','zero-sora','default','{"zero-sora":"sora-2-pro"}','{}','{}');
INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight) VALUES
 ('default','migration-text',51,1,0,1),
 ('default','explicit-zero',51,1,0,1),
 ('default','sora-2',51,1,0,1),
 ('default','opaque-sora-alias',51,0,0,1),
 ('default','sora-similar-undeclared',51,1,0,1),
 ('vip','migration-text',52,0,0,1),
 ('default','opaque-video',54,0,0,1),
 ('default','claude-3-5-sonnet-20240620',55,1,0,1),
 ('default','opaque-task',56,1,0,1),
 ('default','explicit-zero',57,1,0,1),
 ('default','admin-expression',58,1,0,1),
 ('default','missing-price',59,0,0,1),
 ('default','opaque-task-alias',60,0,0,1),
 ('default','audit-custom-model',61,1,0,1),
 ('default','extension-sora',62,0,0,1),
 ('default','zero-sora',63,1,0,1);
INSERT INTO abilities ("group",model,channel_id,enabled,priority,weight,tag) VALUES
 ('source-only','no-provider',99999,NULL,NULL,NULL,NULL),
 ('source-only','模型-é',99998,0,-7,0,'保留来源');
INSERT INTO midjourneys (id,user_id,channel_id,mj_id,code,action,status,progress,quota,submit_time,image_url) VALUES
 (91,41,53,'released-mj-queued',1,'IMAGINE','QUEUED','10%',1200,1700000000000,''),
 (92,42,1053,'released-mj-missing',22,'IMAGINE','QUEUED','0%',2400,1700000000000,''),
 (93,41,53,'released-mj-completed',1,'IMAGINE','SUCCESS','100%',3600,1700000000000,'https://artifact.invalid/released.png');
INSERT INTO tokens (id,user_id,"key",status,name,expired_time,unlimited_quota,model_limits_enabled,model_limits,allow_ips) VALUES
 (61,41,'fixture-restricted-key',1,'restricted',-1,1,1,'migration-alias,unknown-old-model','127.0.0.1'),
 (62,42,'fixture-unrestricted-key',1,'unrestricted',-1,1,0,'','127.0.0.1');
DELETE FROM options WHERE "key" IN ('ModelRatio','ModelPrice','CompletionRatio','billing_setting.billing_mode','billing_setting.billing_expr','billing_setting.plugin_billing_expr');
INSERT INTO options ("key",value) VALUES
 ('ModelRatio','{"migration-text":0.125,"unconvertible":-1,"claude-3-5-sonnet-20240620":1.5,"sora-similar-undeclared":0.25}'),
 ('ModelPrice','{"opaque-video":1,"opaque-task":1,"opaque-task-alias":1,"sora-2":1,"opaque-sora-alias":1,"explicit-zero":0}'),
 ('CompletionRatio','{"migration-text":2}'),
 ('billing_setting.billing_mode','{"admin-expression":"tiered_expr","sora-2":"tiered_expr","audit-custom-model":"tiered_expr","extension-sora":"tiered_expr","zero-sora":"tiered_expr"}'),
 ('billing_setting.billing_expr','{"admin-expression":"tier(\"operator\", fixed(0.25))","sora-2":"0.7","audit-custom-model":"0.7","extension-sora":"0.7","zero-sora":"0.7"}'),
 ('billing_setting.plugin_billing_expr','{"sora::sora-2":"0.2","sora::extension-sora":"0.3","sora::zero-sora":"0"}');
INSERT INTO logs (id,user_id,created_at,type,content,username,model_name,quota,prompt_tokens,completion_tokens,token_id,channel_id,"group",request_id,upstream_request_id,other) VALUES
 (71,41,1700000000,2,'historical fixture record','migration-user-one','migration-text',9007199254740993,100,25,61,51,'default','released-history-71','released-upstream-71','{"public":{"cache_tokens":12,"cache_write_tokens":30,"cache_creation_5m_tokens":10,"cache_creation_1h_tokens":20},"admin":{"channel":51}}'),
 (72,42,1700000100,6,'historical task refund','migration-user-two','opaque-video',-1250,0,0,62,54,'vip','released-history-72','released-upstream-72','{"public":{"task_id":"released-historical-task"}}');
INSERT INTO quota_data (id,user_id,username,model_name,created_at,use_group,token_id,channel_id,node_name,token_used,count,quota,cache_read_tokens,cache_creation_tokens) VALUES
 (81,41,'migration-user-one','migration-text',1699999200,'default',61,51,'retired-node-a',125,2,9007199254740993,12,30),
 (82,42,'migration-user-two','deleted-history-model',1700002800,'vip',999,999,'retired-node-b',0,1,0,0,0);
INSERT INTO prefill_groups (id,name,type,items,deleted_at) VALUES
 (81,'migration-prefill','model','[]','2024-01-01 00:00:00'),
 (82,'migration-prefill','model','[]',NULL);
INSERT INTO prefill_groups (id,name,type,items) VALUES (1000000,'migration-prefill-retired','model','[]');
DELETE FROM prefill_groups WHERE id=1000000;
CREATE TABLE legacy_retained_records (id INTEGER PRIMARY KEY AUTOINCREMENT, record_key TEXT NOT NULL UNIQUE, exact_integer BIGINT NOT NULL, payload BLOB);
INSERT INTO legacy_retained_records(record_key,exact_integer,payload) VALUES ('record-one',9007199254740993,X'00FF10');
`
	pluginSource, err := plugins.Source("sora")
	require.NoError(t, err)
	pluginSource = strings.Replace(pluginSource, `models: ["sora-2", "sora-2-pro"]`, `models: ["sora-2", "sora-2-pro", "audit-custom-model"]`, 1)
	pluginSource = strings.Replace(pluginSource, `version: "1.2.0"`, `version: "9.9.9"`, 1)
	compiledSource, err := jsplugin.CompilePlugin(pluginSource, jsplugin.Options{Key: "sora", Version: "9.9.9"})
	require.NoError(t, err)
	require.Contains(t, compiledSource.Meta.Models, "audit-custom-model")
	pluginHash := sha256.Sum256([]byte(pluginSource))
	// Decode exact UTF-8 bytes without SQL-mode-dependent string escaping.
	sourceLiteral := fmt.Sprintf("CAST(X'%x' AS TEXT)", []byte(pluginSource))
	if kind == "mysql" {
		sourceLiteral = fmt.Sprintf("CONVERT(X'%x' USING utf8mb4)", []byte(pluginSource))
	}
	fixture += fmt.Sprintf("INSERT INTO task_plugins (id,\"key\",api_version,version,source,source_hash,enabled,active,created_at) VALUES (801,'sora',1,'9.9.9',%s,'%x',1,1,1700000000);\n", sourceLiteral, pluginHash)
	seed := exec.Command("sqlite3", source)
	if kind == "mysql" {
		fixture = strings.Replace(fixture, "INTEGER PRIMARY KEY AUTOINCREMENT, record_key TEXT", "BIGINT PRIMARY KEY AUTO_INCREMENT, record_key VARCHAR(64)", 1)
		fixture = strings.Replace(fixture, "(81,'migration-prefill'", "(81,'migration-prefill-deleted'", 1)
		fixture = "SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode,',ANSI_QUOTES,NO_BACKSLASH_ESCAPES');\n" + fixture
		seed = inventory.mysqlCommand(context.Background(), "mysql", "--database="+inventory.database)
	}
	seed.Stdin = strings.NewReader(fixture)
	seed.Stdout, seed.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	require.NoError(t, seed.Run())
	if kind == "mysql" {
		sourceJSON := inventory.mysqlCommand(context.Background(), "mysql", "--database="+inventory.database, "--batch", "--skip-column-names", "--execute=SELECT JSON_VALID(value) FROM options WHERE `key`='billing_setting.billing_expr'")
		result, err := sourceJSON.Output()
		require.NoError(t, err)
		require.Equal(t, "1", strings.TrimSpace(string(result)), "released MySQL fixture must store valid JSON before migration")
	}
	expectedCounts, err := inventory.tableCounts(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(expectedCounts), 39)
	require.NoError(t, inventory.backup(context.Background(), t.TempDir()))
	backupCounts, err := inventory.tableCounts(context.Background())
	require.NoError(t, err)
	require.Equal(t, expectedCounts, backupCounts)
	t.Setenv("LEGACY_MIGRATION_TEST_MYSQL", released)
	args := []string{"--source", kind, "--sqlite-path", source, "--mysql-url-env", "LEGACY_MIGRATION_TEST_MYSQL", "--target-env", "LEGACY_MIGRATION_TEST_TARGET", "--backup-dir", filepath.Join(directory, "backups")}
	var stdout, stderr bytes.Buffer
	var report migrationReport
	// A backup failure must precede any target schema or data changes.
	blockedBackup := filepath.Join(directory, "not-a-directory")
	require.NoError(t, os.WriteFile(blockedBackup, []byte("occupied"), 0o600))
	blockedArgs := append(append([]string{}, args...), "--backup-dir", blockedBackup)
	require.Equal(t, 1, run(context.Background(), blockedArgs, &stdout, &stderr))
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, "backup_directory_failed", report.Failure)
	var changedSchemas int64
	require.NoError(t, db.Raw("SELECT count(*) FROM pg_namespace WHERE nspname = 'gateway_migration' OR nspname LIKE 'legacy_import_%'").Scan(&changedSchemas).Error)
	require.Zero(t, changedSchemas)
	// This test owns the import namespaces it is about to create. Remove them
	// only after all replay/startup assertions, so later isolated-schema tests
	// do not inherit this application's retained import-state evidence.
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`DO $$ DECLARE owned_schema text; BEGIN
		 FOR owned_schema IN SELECT nspname FROM pg_namespace WHERE nspname='gateway_migration' OR left(nspname,14)='legacy_import_' LOOP
		 EXECUTE format('DROP SCHEMA %I CASCADE', owned_schema);
		 END LOOP; END $$`).Error)
	})

	// Each blocked domain transaction must leave the imported legacy data
	// intact. Repairing the imported data then permits the same command to resume.
	for _, test := range []struct {
		name     string
		repair   []string
		resource string
		id       int64
		itemName string
		reason   string
	}{
		{name: "negative balance", resource: "user", id: 41, reason: "negative_legacy_balance"},
		{name: "mapping cycle", repair: []string{
			`UPDATE "users" SET "quota"=500001 WHERE "id"=41`,
			`UPDATE "channels" SET "model_mapping"='{"migration-text":"loop","loop":"migration-text"}' WHERE "id"=51`,
		}, resource: "provider", id: 51, itemName: "loop", reason: "model_mapping_cycle"},
		{name: "channel identity conflict", repair: []string{
			`UPDATE "channels" SET "models"='migration-text,unconvertible,shadow', "model_mapping"='{"migration-text":"upstream-text-v2","shadow":"upstream-text-v2"}' WHERE "id"=51`,
		}, resource: "provider", id: 51, itemName: "shadow", reason: "channel_identity_conflict"},
		{name: "balance overflow after an earlier opening entry", repair: []string{
			`UPDATE "channels" SET "models"='migration-text,unconvertible,explicit-zero,sora-2,opaque-sora-alias,sora-similar-undeclared', "model_mapping"='{"migration-text":"upstream-text-v2","opaque-sora-alias":"SORA-2"}' WHERE "id"=51`,
			`UPDATE "users" SET "quota"=9223372036854775807 WHERE "id"=42`,
		}, resource: "user", id: 42, reason: "legacy_balance_overflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, statement := range test.repair {
				require.NoError(t, db.Exec(statement).Error)
			}
			stdout.Reset()
			stderr.Reset()
			require.Equal(t, 1, run(context.Background(), args, &stdout, &stderr))
			report = migrationReport{}
			require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
			require.Equal(t, "gateway_migration_blocked", report.Failure)
			require.NotNil(t, report.Gateway)
			assert.False(t, report.Gateway.Committed)
			assert.Contains(t, report.Gateway.Items, model.LegacyGatewayMigrationItem{Resource: test.resource, ID: test.id, Name: test.itemName, Reason: test.reason})
			for _, table := range []string{"providers", "model_offerings", "token_offering_policies", "wallets", "ledger_entries"} {
				assert.False(t, db.Migrator().HasTable(table), "blocked domain changes must roll back: %s", table)
			}
			var providers int64
			require.NoError(t, db.Table("channels").Count(&providers).Error)
			assert.Equal(t, int64(13), providers)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
	var invalidQuota int64
	require.NoError(t, db.Table("users").Select("quota").Where("id = ?", 42).Scan(&invalidQuota).Error)
	assert.Equal(t, int64(math.MaxInt64), invalidQuota, "overflow must not clamp the imported source balance")
	require.NoError(t, db.Table("users").Where("id = ?", 42).Update("quota", 0).Error)
	// Freeze all seven imported columns before the successful domain transaction.
	// The same query against the archive must remain equal after the source drops.
	abilityFacts := func(table string) string {
		var state string
		require.Contains(t, []string{"abilities", "legacy_ability_sources"}, table)
		require.NoError(t, db.Raw(`SELECT COALESCE(json_agg(row_to_json(a) ORDER BY channel_id,model,"group")::text,'[]') FROM (SELECT "group",model,channel_id,enabled,priority,weight,tag FROM `+table+`) a`).Scan(&state).Error)
		return state
	}
	originalAbilityFacts := abilityFacts("abilities")
	stdout.Reset()
	stderr.Reset()
	report = migrationReport{}
	code := run(context.Background(), args, &stdout, &stderr)
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	require.Equal(t, 0, code, "migration failed: %s; gateway=%+v", report.Failure, report.Gateway)
	require.Equal(t, "completed", report.Status)
	require.NotNil(t, report.Gateway)
	assert.True(t, report.Gateway.Committed)
	assert.False(t, report.Gateway.AlreadyCompleted)
	assert.GreaterOrEqual(t, len(report.Tables), 39)
	assert.Empty(t, report.Failure)
	sourceBackup := "source.sqlite"
	if kind == "mysql" {
		sourceBackup = "source.mysql.sql"
	}
	for _, name := range []string{"target.dump", sourceBackup, "report.json"} {
		info, err := os.Stat(filepath.Join(report.BackupDirectory, name))
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o077)
		assert.Positive(t, info.Size())
	}
	for _, name := range []string{"source.load", "source-loader.sqlite"} {
		_, err = os.Stat(filepath.Join(report.BackupDirectory, name))
		assert.True(t, os.IsNotExist(err), "temporary loader files must be removed: %s", name)
	}
	if kind == "sqlite" {
		// The retained backup preserves both the original partial predicate and rows.
		backupSource, err := prepareSource("sqlite", filepath.Join(report.BackupDirectory, "source.sqlite"), "")
		require.NoError(t, err)
		partial, err := backupSource.query(context.Background(), `SELECT name FROM pragma_index_list('prefill_groups') WHERE partial=1`)
		require.NoError(t, err)
		require.Len(t, partial, 1)
		assert.Equal(t, "uk_prefill_name", partial[0]["name"])
	}
	for _, secret := range []string{"fixture-provider-one", "fixture-provider-two", "fixture-provider-mj", "fixture-restricted-key", "fixture-unrestricted-key", dsn, released} {
		assert.False(t, bytes.Contains(stdout.Bytes(), []byte(secret)), "report must exclude source credentials")
		assert.False(t, bytes.Contains(stderr.Bytes(), []byte(secret)), "errors must exclude source credentials")
	}
	var obligations []model.Task
	require.NoError(t, db.Where("platform = ?", "midjourney").Order("id").Find(&obligations).Error)
	require.Len(t, obligations, 3, "actual released MJ rows must survive offline SQLite/MySQL import")
	for index, task := range obligations {
		require.NotNil(t, task.PrivateData.LegacyObligation)
		assert.Nil(t, task.PrivateData.Gateway, "historical obligations cannot invent PriceVersions or holds")
		assert.Zero(t, task.Quota)
		assert.Empty(t, task.PrivateData.Key)
		assert.Equal(t, "pending", task.PrivateData.LegacyObligation.Reconciliation)
		assert.Equal(t, "unverified", task.PrivateData.LegacyObligation.DebitEvidence)
		assert.Equal(t, "unverified", task.PrivateData.LegacyObligation.RefundEvidence)
		assert.EqualValues(t, 1200*(index+1), task.PrivateData.LegacyObligation.SourceQuota)
		var source model.Midjourney
		require.NoError(t, db.First(&source, 91+index).Error)
		assert.Equal(t, task.ID, source.MigratedTaskID)
		assert.Equal(t, source.MjId, task.PrivateData.UpstreamTaskID)
		assert.NotEqual(t, source.MjId, task.TaskID)
	}
	assert.Equal(t, 2, obligations[0].PrivateData.LegacyObligation.ProviderType)
	assert.Zero(t, obligations[1].PrivateData.LegacyObligation.ProviderType)
	assert.Equal(t, model.TaskStatus(model.TaskStatusQueued), obligations[1].Status)
	var newBilling int64
	require.NoError(t, db.Model(&model.GatewayBilling{}).Count(&newBilling).Error)
	assert.Zero(t, newBilling, "migration must not reserve or charge historical requests again")
	var offerings []model.ModelOffering
	require.NoError(t, db.Order("id").Find(&offerings).Error)
	require.Len(t, offerings, 15)
	assert.EqualValues(t, 8, report.Gateway.Counts["sale_prices_created"], "safe migration items: %+v", report.Gateway.Items)
	assert.EqualValues(t, 8, report.Gateway.Counts["offerings_enabled"])
	assert.EqualValues(t, 7, report.Gateway.Counts["offerings_disabled"])
	for name, expression := range map[string]string{
		"migration-text":          `tier("base", p * 0.25 + c * 0.5)`,
		"explicit-zero":           `tier("request", fixed(0))`,
		"admin-expression":        `tier("operator", fixed(0.25))`,
		"sora-similar-undeclared": `tier("base", p * 0.5 + c * 0.5)`,
		"sora-2":                  "0.2",
		"audit-custom-model":      "0.7",
		"extension-sora":          "0.3",
		"zero-sora":               "0",
	} {
		var price model.PriceVersion
		require.NoError(t, db.Joins("JOIN model_offerings ON model_offerings.id = price_versions.owner_id").Where("price_versions.owner_type = ? AND model_offerings.name = ?", model.PriceOwnerOffering, name).First(&price).Error)
		assert.Equal(t, expression, price.Expression)
		var enabled model.ModelOffering
		require.NoError(t, db.Where("name = ?", name).First(&enabled).Error)
		assert.True(t, enabled.Enabled, name)
	}
	for _, name := range []string{"opaque-video", "opaque-task", "opaque-task-alias", "opaque-sora-alias", "claude-3-5-sonnet-20240620", "missing-price", "unconvertible"} {
		var denied model.ModelOffering
		require.NoError(t, db.Where("name = ?", name).First(&denied).Error)
		assert.False(t, denied.Enabled, "special routing must not become ordinary text pricing: %s", name)
		assert.Contains(t, report.Gateway.Items, model.LegacyGatewayMigrationItem{Resource: "offering", ID: int64(denied.ID), Name: name, Reason: "sale_price_conversion_failed"})
	}
	assert.False(t, db.Migrator().HasTable("abilities"), "the old routing source must actually be retired")
	assert.JSONEq(t, originalAbilityFacts, abilityFacts("legacy_ability_sources"), "all seven source columns, NULL and orphan identities survive")
	seal, err := model.VerifyLegacyAbilitySourceArchive(db)
	require.NoError(t, err)
	assert.EqualValues(t, 18, seal.RowCount)
	var proof struct {
		Origin         string
		SourceVersion  int
		SourceRowCount int64
		SourceSHA256   string
	}
	require.NoError(t, db.Table("legacy_ability_retirements").First(&proof).Error)
	assert.Equal(t, "legacy", proof.Origin)
	assert.Equal(t, seal.Version, proof.SourceVersion)
	assert.Equal(t, seal.RowCount, proof.SourceRowCount)
	assert.Equal(t, seal.SHA256, proof.SourceSHA256)
	var importedAbilities []model.LegacyAbilitySource
	require.NoError(t, db.Where("channel_id < ?", 99998).Order("channel_id, model").Find(&importedAbilities).Error)
	require.Len(t, importedAbilities, 16)
	falseCount := 0
	for _, ability := range importedAbilities {
		require.NotNil(t, ability.Enabled)
		if !*ability.Enabled {
			falseCount++
		}
	}
	assert.Equal(t, 6, falseCount, "SQLite boolean 0 must not be lost or treated as absent")
	var offering model.ModelOffering
	require.NoError(t, db.Where("name = ?", "migration-text").First(&offering).Error)
	assert.True(t, offering.Enabled)
	assert.Equal(t, "groups", offering.Visibility)
	assert.ElementsMatch(t, []string{"default", "vip"}, offering.Groups)
	assert.ElementsMatch(t, []int{51, 52}, offering.ChannelScope.IncludeProviderIDs)
	var alias model.ModelOfferingName
	require.NoError(t, db.Where("name = ?", "migration-alias").First(&alias).Error)
	assert.Equal(t, offering.ID, alias.OfferingID)
	var disabled model.ModelOffering
	require.NoError(t, db.Where("name = ?", "unconvertible").First(&disabled).Error)
	assert.False(t, disabled.Enabled)
	var channels []model.Channel
	require.NoError(t, db.Order("provider_id, upstream_model_name").Find(&channels).Error)
	require.Len(t, channels, 17)
	for _, channel := range channels {
		assert.Equal(t, model.ChannelPending, channel.Status)
		assert.False(t, channel.MappingConfirmed)
		if channel.ProviderID == 51 {
			expected := "openai_chat"
			if channel.UpstreamModelName == "sora-2" || channel.UpstreamModelName == "SORA-2" {
				expected = "task:sora"
			}
			assert.Equal(t, expected, string(channel.Protocol), channel.UpstreamModelName)
		}
		if channel.ProviderID >= 61 && channel.ProviderID <= 63 {
			assert.Equal(t, "task:sora", string(channel.Protocol), "bound/source-declared task must retain task USD units")
		}
	}
	var sourcePlugin model.TaskPlugin
	require.NoError(t, db.Where("id = ?", 801).First(&sourcePlugin).Error)
	assert.True(t, sourcePlugin.Enabled)
	assert.True(t, sourcePlugin.Active)
	assert.Equal(t, fmt.Sprintf("%x", pluginHash), sourcePlugin.SourceHash)
	assert.Equal(t, pluginHash, sha256.Sum256([]byte(sourcePlugin.Source)))
	for name, nanos := range map[string]int64{"sora-2": 200000000, "audit-custom-model": 700000000, "extension-sora": 300000000, "zero-sora": 0} {
		var price model.PriceVersion
		require.NoError(t, db.Joins("JOIN model_offerings ON model_offerings.id = price_versions.owner_id").Where("price_versions.owner_type = ? AND model_offerings.name = ?", model.PriceOwnerOffering, name).First(&price).Error)
		value, _, err := billingexpr.RunMoneyExpr(price.Expression, billingexpr.MoneyInput{TaskUsage: true}, money.Currency("USD"))
		require.NoError(t, err)
		amount, err := value.Receivable()
		require.NoError(t, err)
		assert.Equal(t, nanos, amount.Nanos, name)
	}
	var actual model.Channel
	require.NoError(t, db.Where("provider_id = ? AND upstream_model_name = ?", 51, "upstream-text-v2").First(&actual).Error)
	assert.JSONEq(t, `{"temperature":0}`, actual.ParameterOverrides)
	var policy model.TokenOfferingPolicy
	require.NoError(t, db.Where("token_id = ?", 61).First(&policy).Error)
	assert.False(t, policy.DefaultAllow)
	var exceptions []model.TokenOfferingException
	require.NoError(t, db.Where("token_id = ?", 61).Find(&exceptions).Error)
	require.Len(t, exceptions, 1)
	assert.Equal(t, offering.ID, exceptions[0].OfferingID)
	var unrestricted int64
	require.NoError(t, db.Model(&model.TokenOfferingPolicy{}).Where("token_id = ?", 62).Count(&unrestricted).Error)
	assert.Zero(t, unrestricted)
	assert.Contains(t, report.Gateway.Items, model.LegacyGatewayMigrationItem{Resource: "key", ID: 61, Name: "unknown-old-model", Reason: "unknown_model_limit_name"})
	var wallet model.Wallet
	require.NoError(t, db.Where("owner_id = ? AND account_type = ?", 41, model.LedgerAccountUserWallet).First(&wallet).Error)
	assert.Equal(t, money.Amount{Nanos: 1000002000, Currency: "USD"}, wallet.Balance)
	var entries []model.LedgerEntry
	require.NoError(t, db.Where("type = ?", model.LedgerEntryOpeningBalance).Find(&entries).Error)
	require.Len(t, entries, 1)
	assert.Equal(t, wallet.Balance, entries[0].Amount)
	var retained struct {
		ExactInteger int64
		Payload      []byte
	}
	require.NoError(t, db.Table("legacy_retained_records").First(&retained).Error)
	assert.Equal(t, int64(9007199254740993), retained.ExactInteger)
	assert.Equal(t, []byte{0, 255, 16}, retained.Payload)
	assert.Error(t, db.Exec("INSERT INTO legacy_retained_records(record_key,exact_integer) VALUES ('record-one',1)").Error, "source uniqueness must survive transfer")
	var historical string
	require.NoError(t, db.Raw("SELECT content FROM logs WHERE id=71").Scan(&historical).Error)
	assert.Equal(t, "historical fixture record", historical)
	var historicalQuota []model.QuotaData
	require.NoError(t, db.Order("id").Find(&historicalQuota).Error)
	assert.Equal(t, []model.QuotaData{
		{Id: 81, UserID: 41, Username: "migration-user-one", ModelName: "migration-text", CreatedAt: 1699999200, UseGroup: "default", TokenID: 61, ChannelID: 51, NodeName: "retired-node-a", TokenUsed: 125, Count: 2, Quota: 9007199254740993, CacheReadTokens: 12, CacheCreationTokens: 30},
		{Id: 82, UserID: 42, Username: "migration-user-two", ModelName: "deleted-history-model", CreatedAt: 1700002800, UseGroup: "vip", TokenID: 999, ChannelID: 999, NodeName: "retired-node-b", Count: 1},
	}, historicalQuota, "history remains history, including large integers, deleted dimensions and normalized cache totals")
	var historicalLogs []struct {
		ID                int
		Quota             string
		PromptTokens      int
		CompletionTokens  int
		RequestID         string
		UpstreamRequestID string
		Other             string
	}
	require.NoError(t, db.Raw("SELECT id,quota::text AS quota,prompt_tokens,completion_tokens,request_id,upstream_request_id,other FROM logs ORDER BY id").Scan(&historicalLogs).Error)
	require.Len(t, historicalLogs, 2)
	assert.Equal(t, "9007199254740993", historicalLogs[0].Quota)
	assert.Equal(t, 100, historicalLogs[0].PromptTokens)
	assert.Equal(t, 25, historicalLogs[0].CompletionTokens)
	assert.Equal(t, "released-history-71", historicalLogs[0].RequestID)
	assert.Equal(t, "released-upstream-71", historicalLogs[0].UpstreamRequestID)
	assert.JSONEq(t, `{"public":{"cache_tokens":12,"cache_write_tokens":30,"cache_creation_5m_tokens":10,"cache_creation_1h_tokens":20},"admin":{"channel":51}}`, historicalLogs[0].Other)
	assert.Equal(t, "-1250", historicalLogs[1].Quota)
	assert.JSONEq(t, `{"public":{"task_id":"released-historical-task"}}`, historicalLogs[1].Other)
	var prefill []model.PrefillGroup
	require.NoError(t, db.Unscoped().Where("name LIKE ?", "migration-prefill%").Order("id").Find(&prefill).Error)
	require.Len(t, prefill, 2)
	assert.True(t, prefill[0].DeletedAt.Valid)
	assert.False(t, prefill[1].DeletedAt.Valid)
	assert.Error(t, db.Create(&model.PrefillGroup{Name: "migration-prefill", Type: "model", Items: model.JSONValue(`[]`)}).Error, "active names must remain unique")
	require.NoError(t, db.Delete(&prefill[1]).Error)
	replacement := model.PrefillGroup{Name: "migration-prefill", Type: "model", Items: model.JSONValue(`[]`)}
	require.NoError(t, db.Create(&replacement).Error)
	assert.Greater(t, replacement.Id, 1000000, "import must not reuse a deleted source AUTOINCREMENT identity")
	require.NoError(t, db.Model(&model.ModelOffering{}).Where("id = ?", offering.ID).Update("enabled", false).Error)
	require.NoError(t, db.Model(&model.TokenOfferingPolicy{}).Where("token_id = ?", 61).Update("default_allow", true).Error)
	var reminderPreferences []model.UserBalanceReminder
	require.NoError(t, db.Order("user_id").Find(&reminderPreferences).Error)
	require.Len(t, reminderPreferences, 2)
	assert.False(t, reminderPreferences[0].Enabled)
	require.NotNil(t, reminderPreferences[0].ThresholdNanos)
	assert.EqualValues(t, 1, *reminderPreferences[0].ThresholdNanos, "old fractional comparison threshold rounds up exactly once")
	assert.Equal(t, money.Currency("USD"), *reminderPreferences[0].ThresholdCurrency)
	assert.True(t, reminderPreferences[1].Enabled)
	assert.Nil(t, reminderPreferences[1].ThresholdNanos, "old zero inherits; it is not an explicit zero")
	var reminderSite model.Option
	require.NoError(t, db.Where("key = ?", "balance_reminder_setting.config").Take(&reminderSite).Error)
	assert.JSONEq(t, `{"enabled":true,"threshold":{"nanos":"2000000","currency":"USD"}}`, reminderSite.Value)
	var reminderCount int64
	require.NoError(t, db.Table("balance_reminder_events").Count(&reminderCount).Error)
	assert.Zero(t, reminderCount, "source migration must not replay old consumption notifications")
	var oldReminderSource model.Option
	require.NoError(t, db.Where("key = ?", "QuotaRemindThreshold").Take(&oldReminderSource).Error)
	assert.Equal(t, "1000", oldReminderSource.Value)
	catalogState := func() string {
		var state string
		require.NoError(t, db.Raw(`SELECT jsonb_build_object(
		 'channels', (SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM channels c),
		 'prices', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM price_versions p),
		 'offerings', (SELECT jsonb_agg(to_jsonb(o) ORDER BY id) FROM model_offerings o),
		 'abilities', (SELECT jsonb_agg(to_jsonb(a) ORDER BY ordinal) FROM legacy_ability_sources a),
		 'ability_seal', (SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM legacy_ability_source_manifests s),
		 'ability_retirement', (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM legacy_ability_retirements r),
		 'historical_quota_data', (SELECT jsonb_agg(to_jsonb(q) ORDER BY id) FROM quota_data q),
		 'historical_logs', (SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM logs l),
		 'subscription_plans', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM subscription_plans p),
		 'subscriptions', (SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM user_subscriptions s),
		 'subscription_periods', (SELECT jsonb_agg(to_jsonb(p) ORDER BY id) FROM subscription_periods p),
		 'subscription_openings', (SELECT jsonb_agg(to_jsonb(o) ORDER BY subscription_id) FROM subscription_money_openings o),
		 'subscription_plan_openings', (SELECT jsonb_agg(to_jsonb(o) ORDER BY plan_id) FROM subscription_plan_money_openings o),
		 'subscription_obligations', (SELECT jsonb_agg(to_jsonb(o) ORDER BY source_record_id) FROM legacy_subscription_obligations o),
		 'subscription_task_obligations', (SELECT jsonb_agg(to_jsonb(o) ORDER BY source_task_id) FROM legacy_subscription_task_obligations o),
		 'legacy_task_obligations', (SELECT jsonb_agg(to_jsonb(o) ORDER BY source_task_id) FROM legacy_task_obligations o),
		 'legacy_task_sources', (SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM tasks t WHERE id BETWEEN 111 AND 114),
		 'ledger', (SELECT jsonb_agg(to_jsonb(e) ORDER BY id) FROM ledger_entries e),
		 'reminder_preferences', (SELECT jsonb_agg(to_jsonb(r) ORDER BY user_id) FROM user_balance_reminders r),
		 'reminder_events', (SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM balance_reminder_events r),
		 'reminder_options', (SELECT jsonb_agg(to_jsonb(o) ORDER BY key) FROM options o WHERE key IN ('QuotaRemindThreshold','QuotaRemindEnabled','balance_reminder_setting.config')),
		 'reminder_sources', (SELECT jsonb_agg(jsonb_build_object('id',id,'setting',setting) ORDER BY id) FROM users),
		 'plugins', (SELECT jsonb_agg(jsonb_build_object('key',key,'version',version,'source_hash',source_hash,'enabled',enabled,'active',active) ORDER BY id) FROM task_plugins)
		)::text`).Scan(&state).Error)
		return state
	}
	previous, previousLog, previousMaster := model.DB, model.LOG_DB, common.IsMasterNode
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.IsMasterNode = previous, previousLog, previousMaster
	})
	t.Setenv("SQL_DSN", dsn)
	logDSN := testdb.ClickHouseLogDSN(t)
	t.Setenv("LOG_SQL_DSN", logDSN)
	catalogBeforeLate := catalogState()
	existingBeforeLate := midjourneyMigrationState(t, db)
	late := model.Midjourney{Id: 94, UserId: 987, ChannelId: 9999, MjId: "released-late-terminal", Action: "IMAGINE", Status: "SUCCESS", Progress: "100%", Quota: -17}
	require.NoError(t, db.Create(&late).Error)
	beforeRejectedStartup := midjourneyMigrationState(t, db)
	for _, master := range []bool{true, false} {
		common.IsMasterNode = master
		assert.Error(t, model.InitDB(), "a completed marker cannot authorize an unimported terminal source")
		pool, err := model.DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		assert.Equal(t, beforeRejectedStartup, midjourneyMigrationState(t, db), "normal startup remains read-only on refusal")
	}
	stdout.Reset()
	stderr.Reset()
	require.Zero(t, run(context.Background(), args, &stdout, &stderr))
	report = migrationReport{}
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	require.NotNil(t, report.Gateway)
	assert.True(t, report.Gateway.Committed)
	assert.False(t, report.Gateway.AlreadyCompleted)
	assert.EqualValues(t, 1, report.Gateway.Counts["midjourney_obligations"])
	assert.Zero(t, report.Gateway.Counts["sale_prices_created"])
	assert.Zero(t, report.Gateway.Counts["offerings_enabled"])
	assert.JSONEq(t, catalogBeforeLate, catalogState(), "incremental MJ import cannot replay catalog, subscription, price or ledger migration")
	for table, digest := range existingBeforeLate {
		if table != "midjourneys" && table != "tasks" {
			assert.Equal(t, digest, midjourneyMigrationState(t, db)[table], table)
		}
	}
	for _, original := range obligations {
		var retained model.Task
		require.NoError(t, db.First(&retained, original.ID).Error)
		assert.Equal(t, original, retained, "previous canonical Tasks and evidence cannot be overwritten")
	}
	require.NoError(t, db.First(&late, late.Id).Error)
	var lateTask model.Task
	require.NoError(t, db.First(&lateTask, late.MigratedTaskID).Error)
	require.NotNil(t, lateTask.PrivateData.LegacyObligation)
	assert.EqualValues(t, -17, lateTask.PrivateData.LegacyObligation.SourceQuota)
	assert.Zero(t, lateTask.PrivateData.LegacyObligation.ProviderType, "missing Provider cannot become a pollable identity")
	assert.Equal(t, "unverified", lateTask.PrivateData.LegacyObligation.DebitEvidence)
	assert.Nil(t, lateTask.PrivateData.Gateway)
	assert.Zero(t, lateTask.Quota)
	require.NoError(t, model.ValidateLegacyMidjourneyMigration(db))
	afterIncremental := midjourneyMigrationState(t, db)
	beforeRepeat := catalogState()
	stdout.Reset()
	stderr.Reset()
	require.Zero(t, run(context.Background(), args, &stdout, &stderr))
	report = migrationReport{}
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	require.NotNil(t, report.Gateway)
	assert.True(t, report.Gateway.AlreadyCompleted)
	assert.Zero(t, report.Gateway.Counts["sale_prices_created"])
	assert.Zero(t, report.Gateway.Counts["offerings_enabled"])
	var priceCount int64
	require.NoError(t, db.Model(&model.PriceVersion{}).Where("owner_type = ?", model.PriceOwnerOffering).Count(&priceCount).Error)
	assert.EqualValues(t, 8, priceCount)
	assert.JSONEq(t, beforeRepeat, catalogState(), "migration replay must not rewrite route or price facts")
	require.NoError(t, db.First(&wallet, wallet.Id).Error)
	assert.Equal(t, int64(1000002000), wallet.Balance.Nanos)
	require.NoError(t, db.First(&offering, offering.ID).Error)
	assert.False(t, offering.Enabled, "replay must preserve subsequent administrator changes")
	var openingCount int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryOpeningBalance).Count(&openingCount).Error)
	assert.Equal(t, int64(1), openingCount)
	for _, master := range []bool{true, true, false, false} {
		common.IsMasterNode = master
		require.NoError(t, model.InitDB(), "normal startup must accept the migrated database")
		assert.False(t, db.Migrator().HasTable("abilities"), "neither startup role may recreate the retired source")
		assert.JSONEq(t, originalAbilityFacts, abilityFacts("legacy_ability_sources"))
		pool, err := model.DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		require.NoError(t, model.InitLogDB())
		if logDSN != "" {
			logPool, err := model.LOG_DB.DB()
			require.NoError(t, err)
			t.Cleanup(func() { _ = logPool.Close() })
			assert.Equal(t, "clickhouse", model.LOG_DB.Dialector.Name())
			assert.False(t, model.LOG_DB.Migrator().HasTable("price_versions"))
			assert.False(t, model.LOG_DB.Migrator().HasTable("abilities"))
		}
	}
	assert.JSONEq(t, beforeRepeat, catalogState(), "two startups per role must preserve migrated route and price facts")
	assert.Equal(t, afterIncremental, midjourneyMigrationState(t, db), "replay and both startup roles must preserve all old and newly imported MJ facts")
	require.NoError(t, db.Where("token_id = ?", 61).First(&policy).Error)
	assert.True(t, policy.DefaultAllow, "replay and normal startup must preserve administrator permission changes")
	require.NoError(t, db.Raw("SELECT content FROM logs WHERE id=71").Scan(&historical).Error)
	assert.Equal(t, "historical fixture record", historical)
	var obligationCount int64
	require.NoError(t, db.Model(&model.Task{}).Where("platform = ?", "midjourney").Count(&obligationCount).Error)
	assert.EqualValues(t, 4, obligationCount, "incremental import creates exactly one obligation; replay and startup must not duplicate it")
	model.DB = db
	var subscriptionObligations []model.LegacySubscriptionObligation
	require.NoError(t, db.Order("source_record_id").Find(&subscriptionObligations).Error)
	require.Len(t, subscriptionObligations, 4)
	for index, obligation := range subscriptionObligations {
		assert.Equal(t, 901+index, obligation.SourceRecordID)
		assert.Equal(t, []string{"released-subscription-request", "released-subscription-refunded", "released-subscription-cancelled", "released-subscription-deleted-instance"}[index], obligation.RequestID)
		assert.Equal(t, []int{41, 41, 42, 41}[index], obligation.UserID)
		assert.Equal(t, []int{701, 702, 703, 799}[index], obligation.SubscriptionID)
		assert.Equal(t, []int64{100000, 1000, 1000, 1250}[index], obligation.SourcePreConsumed)
		assert.Equal(t, []string{"consumed", "refunded", "consumed", "consumed"}[index], obligation.SourceStatus)
		assert.Equal(t, []string{"unverified", "source_refunded", "unverified", "unverified"}[index], obligation.State)
	}
	for _, table := range []string{"user_subscriptions", "subscription_periods", "subscription_money_openings"} {
		var count int64
		require.NoError(t, db.Table(table).Count(&count).Error)
		assert.EqualValues(t, 4, count, "deleted historical instances must not be recreated: %s", table)
	}
	var taskObligations []model.LegacySubscriptionTaskObligation
	require.NoError(t, db.Order("source_task_id").Find(&taskObligations).Error)
	require.Len(t, taskObligations, 3)
	for index, obligation := range taskObligations {
		assert.EqualValues(t, 101+index, obligation.SourceTaskID)
		assert.Equal(t, []int{41, 42, 42}[index], obligation.UserID)
		assert.Equal(t, []int{701, 704, 703}[index], obligation.SubscriptionID)
		assert.Equal(t, 51, obligation.ProviderID)
		assert.Equal(t, "sora", obligation.Platform)
		assert.Equal(t, []string{"released-subscription-task", "released-subscription-cleaned-evidence", "released-subscription-failed"}[index], obligation.TaskID)
		assert.Equal(t, []string{"released-subscription-request", "", ""}[index], obligation.RequestID, "missing original request identity must not be replaced with task ID")
		assert.Equal(t, []int64{100000, 0, 1000}[index], obligation.SourceQuota)
		assert.Equal(t, []string{"IN_PROGRESS", "SUCCESS", "FAILURE"}[index], obligation.SourceStatus)
		assert.Equal(t, "unverified", obligation.State, "terminal task status is not proof of final financial settlement")
	}
	var evidenceJSON string
	require.NoError(t, db.Raw(`SELECT jsonb_agg(to_jsonb(o))::text FROM "legacy_subscription_task_obligations" o`).Scan(&evidenceJSON).Error)
	assert.NotContains(t, evidenceJSON, "fixture-retained-task-key")
	assert.NotContains(t, evidenceJSON, "private_data")
	for _, statement := range []string{
		`UPDATE "legacy_subscription_task_obligations" SET "source_quota"=0`,
		`DELETE FROM "legacy_subscription_task_obligations"`,
		`TRUNCATE "legacy_subscription_task_obligations"`,
	} {
		err := db.Transaction(func(tx *gorm.DB) error { return tx.Exec(statement).Error })
		assert.ErrorContains(t, err, "legacy_subscription_task_obligations is immutable")
	}
	var ordinaryObligations []model.LegacyTaskObligation
	require.NoError(t, db.Order("source_task_id").Find(&ordinaryObligations).Error)
	require.Len(t, ordinaryObligations, 4, "ordinary archive must not duplicate Gateway, Midjourney or subscription obligations")
	for index, obligation := range ordinaryObligations {
		assert.EqualValues(t, 111+index, obligation.SourceTaskID)
		assert.Equal(t, []int{41, 42, 987, 0}[index], obligation.UserID)
		assert.Equal(t, []int{51, 51, 988, 0}[index], obligation.ProviderID)
		assert.Equal(t, []int{61, 0, 989, 0}[index], obligation.TokenID)
		assert.Equal(t, []int{0, 0, 990, 0}[index], obligation.SubscriptionID)
		assert.Equal(t, []string{"released-wallet-task", "released-empty-source-task", "released-unknown-source-task", ""}[index], obligation.TaskID)
		assert.Equal(t, []string{"released-private-task", "", "", ""}[index], obligation.UpstreamTaskID)
		assert.Equal(t, []string{"released-wallet-request", "", "", ""}[index], obligation.RequestID)
		assert.Equal(t, []string{"wallet", "", "unknown-source", ""}[index], obligation.BillingSource)
		assert.Equal(t, []int64{2500, 0, 1250, 0}[index], obligation.SourceQuota)
		assert.Equal(t, []string{"IN_PROGRESS", "SUCCESS", "FAILURE", "IN_PROGRESS"}[index], obligation.SourceStatus)
		assert.Equal(t, "sora", obligation.Platform)
		assert.Equal(t, "unverified", obligation.State, "terminal status or zero quota cannot establish money settlement")
		if index == 0 {
			assert.Equal(t, "sora", obligation.PluginKey)
			assert.Equal(t, "source-version", obligation.PluginVersion)
			assert.Equal(t, uint64(math.MaxUint64), obligation.PluginGeneration)
		} else {
			assert.Empty(t, obligation.PluginKey)
			assert.Empty(t, obligation.PluginVersion)
			assert.Zero(t, obligation.PluginGeneration)
		}
	}
	require.NoError(t, db.Raw(`SELECT jsonb_agg(to_jsonb(o))::text FROM "legacy_task_obligations" o`).Scan(&evidenceJSON).Error)
	assert.NotContains(t, evidenceJSON, "fixture-ordinary-task-key")
	assert.NotContains(t, evidenceJSON, "private_data")
	var retainedOrdinaryTask model.Task
	require.NoError(t, db.First(&retainedOrdinaryTask, 111).Error)
	assert.Equal(t, "fixture-ordinary-task-key", retainedOrdinaryTask.PrivateData.Key, "archiving must not rewrite the original compatibility credential")
	assert.Equal(t, 2500, retainedOrdinaryTask.Quota)
	for _, statement := range []string{
		`UPDATE "legacy_task_obligations" SET "source_quota"=0`,
		`DELETE FROM "legacy_task_obligations"`,
		`TRUNCATE "legacy_task_obligations"`,
	} {
		err := db.Transaction(func(tx *gorm.DB) error { return tx.Exec(statement).Error })
		assert.ErrorContains(t, err, "legacy task evidence is immutable")
	}
	for table, id := range map[string]int{"users": 987, "providers": 988, "tokens": 989, "user_subscriptions": 990} {
		var count int64
		require.NoError(t, db.Table(table).Where("id = ?", id).Count(&count).Error)
		assert.Zero(t, count, "missing source parents must not be fabricated: %s", table)
	}
	pendingReconciliation, err := model.GetLegacySubscriptionReconciliationPending(db, []int{701, 702, 703, 704})
	require.NoError(t, err)
	assert.Equal(t, map[int]bool{701: true, 702: true, 703: true, 704: true}, pendingReconciliation)
	var migratedPlan model.SubscriptionPlan
	require.NoError(t, db.First(&migratedPlan, 701).Error)
	assert.Equal(t, money.Amount{Nanos: 17123456000, Currency: "EUR"}, migratedPlan.Price)
	assert.Equal(t, money.Amount{Nanos: 1500000000, Currency: "USD"}, migratedPlan.Allowance)
	assert.Equal(t, 1, migratedPlan.MoneyVersion)
	for index, id := range []int{701, 702, 703, 704} {
		period, err := model.GetSubscriptionPeriodSummary(t.Context(), id)
		require.NoError(t, err)
		assert.EqualValues(t, 1, period.Sequence)
		assert.Equal(t, []int64{250000000, 1000000000, 1000000000, 500000000}[index], period.Consumed.Nanos)
		assert.Equal(t, []int64{750000000, 1000000000, 0, 0}[index], period.Available.Nanos)
		assert.Equal(t, index == 3, period.Unlimited)
		assert.Zero(t, period.Reserved.Nanos)
		var original model.UserSubscription
		require.NoError(t, db.First(&original, id).Error)
		assert.Equal(t, []string{"active", "expired", "cancelled", "active"}[index], original.Status)
		assert.Equal(t, index == 1 || index == 3, original.AllowWalletOverflow)
	}
	plan := model.SubscriptionPlan{
		Title: "Purchase after released upgrade", Price: money.Amount{Nanos: 1000000000, Currency: "USD"},
		Allowance: money.Amount{Nanos: 2000000000, Currency: "USD"}, MoneyVersion: 1,
		Enabled: true, AllowBalancePay: common.GetPointer(true), AllowWalletOverflow: common.GetPointer(false),
		DurationUnit: "month", DurationValue: 1, QuotaResetPeriod: "never",
	}
	require.NoError(t, db.Create(&plan).Error)
	var sourceQuota int64
	require.NoError(t, db.Raw(`SELECT "quota" FROM "users" WHERE "id"=41`).Scan(&sourceQuota).Error)
	assert.EqualValues(t, 500001, sourceQuota)
	var ordersBefore int64
	require.NoError(t, db.Model(&model.SubscriptionOrder{}).Count(&ordersBefore).Error)
	const purchaseID = "8ed855b0-7405-4f04-b631-c1af9fe8991b"
	purchaseRevision, err := model.SubscriptionPurchaseRevision(&plan)
	require.NoError(t, err)
	require.NotEmpty(t, purchaseRevision)
	purchase, err := model.PurchaseSubscriptionWithBalance(t.Context(), 41, plan.Id, purchaseID, purchaseRevision)
	require.NoError(t, err)
	replayed, err := model.PurchaseSubscriptionWithBalance(t.Context(), 41, plan.Id, purchaseID, purchaseRevision)
	require.NoError(t, err)
	assert.Equal(t, purchase.OrderID, replayed.OrderID)
	assert.Equal(t, plan.Price, purchase.ContractPrice)
	assert.Equal(t, plan.Price, purchase.WalletCharge)
	assert.False(t, purchase.FXRate.Valid)
	require.NotNil(t, purchase.Period)
	assert.Equal(t, plan.Allowance, purchase.Period.Available)
	assert.Zero(t, purchase.Period.Consumed.Nanos)
	assert.Zero(t, purchase.Period.Reserved.Nanos)
	postPurchase := catalogState()
	converted, err := model.MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	assert.True(t, converted.AlreadyCompleted)
	assert.JSONEq(t, postPurchase, catalogState(), "repeating migration must not restore spent funds or reset the purchased period")
	_, err = model.PurchaseSubscriptionWithBalance(t.Context(), 41, plan.Id, "c7aa5cc1-b217-44af-89f9-504624ac9c7c", purchaseRevision)
	assert.EqualError(t, err, "Insufficient balance", "retained source quota cannot buy the subscription a second time")
	var updatedWallet model.Wallet
	require.NoError(t, db.First(&updatedWallet, wallet.Id).Error)
	assert.Equal(t, money.Amount{Nanos: 2000, Currency: "USD"}, updatedWallet.Balance)
	require.NoError(t, db.Raw(`SELECT "quota" FROM "users" WHERE "id"=41`).Scan(&sourceQuota).Error)
	assert.EqualValues(t, 500001, sourceQuota, "canonical purchases must not dual-write historical quota")
	var ordersAfter, revenueEntries int64
	require.NoError(t, db.Model(&model.SubscriptionOrder{}).Count(&ordersAfter).Error)
	assert.Equal(t, ordersBefore+1, ordersAfter)
	require.NoError(t, db.Table("ledger_entries AS e").Joins("JOIN wallets AS w ON w.id=e.wallet_id").Where("w.account_type = ?", model.LedgerAccountPlatformRevenue).Count(&revenueEntries).Error)
	assert.Zero(t, revenueEntries, "purchase grants allowance without recognizing request revenue twice")
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}
