package legacymigrate

import (
	"bytes"
	"context"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/testdb"
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

func testLegacyMigrationReleasedDatabase(t *testing.T, kind, released string) {
	t.Helper()
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
INSERT INTO channels (id,type,"key",status,name,models,"group",model_mapping,param_override) VALUES
 (51,1,'fixture-provider-one',1,'migration-provider-one','migration-text,unconvertible','default','{"migration-text":"upstream-text-v2"}','{"temperature":0}'),
 (52,1,'fixture-provider-two',1,'migration-provider-two','migration-text','vip','{"migration-alias":"migration-text"}','{}');
INSERT INTO tokens (id,user_id,"key",status,name,expired_time,unlimited_quota,model_limits_enabled,model_limits,allow_ips) VALUES
 (61,41,'fixture-restricted-key',1,'restricted',-1,1,1,'migration-alias,unknown-old-model','127.0.0.1'),
 (62,42,'fixture-unrestricted-key',1,'unrestricted',-1,1,0,'','127.0.0.1');
DELETE FROM options WHERE "key" IN ('ModelRatio','CompletionRatio');
INSERT INTO options ("key",value) VALUES
 ('ModelRatio','{"migration-text":0.125,"unconvertible":-1}'),
 ('CompletionRatio','{"migration-text":2}');
INSERT INTO logs (id,user_id,created_at,type,content,model_name) VALUES
 (71,41,1700000000,2,'historical fixture record','migration-text');
INSERT INTO prefill_groups (id,name,type,items,deleted_at) VALUES
 (81,'migration-prefill','model','[]','2024-01-01 00:00:00'),
 (82,'migration-prefill','model','[]',NULL);
INSERT INTO prefill_groups (id,name,type,items) VALUES (1000000,'migration-prefill-retired','model','[]');
DELETE FROM prefill_groups WHERE id=1000000;
CREATE TABLE legacy_retained_records (id INTEGER PRIMARY KEY AUTOINCREMENT, record_key TEXT NOT NULL UNIQUE, exact_integer BIGINT NOT NULL, payload BLOB);
INSERT INTO legacy_retained_records(record_key,exact_integer,payload) VALUES ('record-one',9007199254740993,X'00FF10');
`
	seed := exec.Command("sqlite3", source)
	if kind == "mysql" {
		fixture = strings.Replace(fixture, "INTEGER PRIMARY KEY AUTOINCREMENT, record_key TEXT", "BIGINT PRIMARY KEY AUTO_INCREMENT, record_key VARCHAR(64)", 1)
		fixture = strings.Replace(fixture, "(81,'migration-prefill'", "(81,'migration-prefill-deleted'", 1)
		fixture = "SET SESSION sql_mode=CONCAT(@@SESSION.sql_mode,',ANSI_QUOTES');\n" + fixture
		seed = inventory.mysqlCommand(context.Background(), "mysql", "--database="+inventory.database)
	}
	seed.Stdin = strings.NewReader(fixture)
	seed.Stdout, seed.Stderr = &bytes.Buffer{}, &bytes.Buffer{}
	require.NoError(t, seed.Run())
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
	assert.Zero(t, changedSchemas)

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
			`UPDATE "channels" SET "models"='migration-text,unconvertible', "model_mapping"='{"migration-text":"upstream-text-v2"}' WHERE "id"=51`,
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
			assert.Equal(t, int64(2), providers)
		})
		if t.Failed() {
			t.FailNow()
		}
	}
	var invalidQuota int64
	require.NoError(t, db.Table("users").Select("quota").Where("id = ?", 42).Scan(&invalidQuota).Error)
	assert.Equal(t, int64(math.MaxInt64), invalidQuota, "overflow must not clamp the imported source balance")
	require.NoError(t, db.Table("users").Where("id = ?", 42).Update("quota", 0).Error)
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
	for _, secret := range []string{"fixture-provider-one", "fixture-provider-two", "fixture-restricted-key", "fixture-unrestricted-key", dsn, released} {
		assert.False(t, bytes.Contains(stdout.Bytes(), []byte(secret)), "report must exclude source credentials")
		assert.False(t, bytes.Contains(stderr.Bytes(), []byte(secret)), "errors must exclude source credentials")
	}
	var offerings []model.ModelOffering
	require.NoError(t, db.Order("id").Find(&offerings).Error)
	require.Len(t, offerings, 2)
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
	require.Len(t, channels, 3)
	for _, channel := range channels {
		assert.Equal(t, model.ChannelPending, channel.Status)
		assert.False(t, channel.MappingConfirmed)
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
	stdout.Reset()
	stderr.Reset()
	require.Zero(t, run(context.Background(), args, &stdout, &stderr))
	require.NoError(t, common.Unmarshal(stdout.Bytes(), &report))
	require.NotNil(t, report.Gateway)
	assert.True(t, report.Gateway.AlreadyCompleted)
	require.NoError(t, db.First(&wallet, wallet.Id).Error)
	assert.Equal(t, int64(1000002000), wallet.Balance.Nanos)
	require.NoError(t, db.First(&offering, offering.ID).Error)
	assert.False(t, offering.Enabled, "replay must preserve subsequent administrator changes")
	var openingCount int64
	require.NoError(t, db.Model(&model.LedgerEntry{}).Where("type = ?", model.LedgerEntryOpeningBalance).Count(&openingCount).Error)
	assert.Equal(t, int64(1), openingCount)
	previous, previousLog, previousMaster := model.DB, model.LOG_DB, common.IsMasterNode
	t.Cleanup(func() {
		model.DB, model.LOG_DB, common.IsMasterNode = previous, previousLog, previousMaster
	})
	common.IsMasterNode = true
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	for range 2 {
		require.NoError(t, model.InitDB(), "normal startup must accept the migrated database")
		pool, err := model.DB.DB()
		require.NoError(t, err)
		t.Cleanup(func() { _ = pool.Close() })
		require.NoError(t, model.InitLogDB())
	}
	require.NoError(t, db.Where("token_id = ?", 61).First(&policy).Error)
	assert.True(t, policy.DefaultAllow, "replay and normal startup must preserve administrator permission changes")
	require.NoError(t, db.Raw("SELECT content FROM logs WHERE id=71").Scan(&historical).Error)
	assert.Equal(t, "historical fixture record", historical)
	model.DB = db
	reconciliation, err := model.ReconcileWallets(100)
	require.NoError(t, err)
	assert.Empty(t, reconciliation.Mismatches)
}
