// Package legacymigrate implements the offline legacy database migration command.
package legacymigrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/jackc/pgx/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// RunCLI runs before application initialization, without listeners or workers.
func RunCLI(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, args, stdout, stderr)
}

type migrationReport struct {
	Status          string                              `json:"status"`
	Source          string                              `json:"source"`
	BackupDirectory string                              `json:"backup_directory,omitempty"`
	Failure         string                              `json:"failure,omitempty"`
	Tables          map[string]int64                    `json:"source_tables,omitempty"`
	Gateway         *model.LegacyGatewayMigrationReport `json:"gateway,omitempty"`
}

// sourceImport records ownership of a staging schema, independently of the old
// application's tables. Promotion and the imported marker commit together.
type sourceImport struct {
	ID          int    `gorm:"primaryKey"`
	Fingerprint string `gorm:"not null"`
	Schema      string `gorm:"not null"`
	Phase       string `gorm:"not null"`
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("new-api migrate-legacy", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "postgres", "legacy source: postgres (in place), sqlite, or mysql")
	sqlitePath := flags.String("sqlite-path", "", "legacy SQLite database file")
	mysqlEnv := flags.String("mysql-url-env", "LEGACY_MYSQL_URL", "environment variable containing the source MySQL URL")
	targetEnv := flags.String("target-env", "SQL_DSN", "environment variable containing the target PostgreSQL URL")
	backupRoot := flags.String("backup-dir", "", "directory for private automatic backups and the JSON report")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: new-api migrate-legacy --backup-dir <directory> [--source postgres|sqlite|mysql]")
		fmt.Fprintln(stderr, "Stop every application node before running this offline command. PostgreSQL client tools are required; cross-database migration also requires pgloader and the source database client.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || *backupRoot == "" {
		flags.Usage()
		return 2
	}
	report := migrationReport{Status: "failed"}
	fail := func(reason string) int {
		report.Failure = reason
		writeReport(report, stdout)
		return 1
	}
	if *source != "postgres" && *source != "sqlite" && *source != "mysql" {
		return fail("unsupported_source")
	}
	report.Source = *source
	targetURL, err := url.Parse(os.Getenv(*targetEnv))
	if err != nil || targetURL == nil || (targetURL.Scheme != "postgres" && targetURL.Scheme != "postgresql") || targetURL.Hostname() == "" || targetURL.Path == "" {
		return fail("invalid_target_connection")
	}
	if _, err := pgx.ParseConfig(targetURL.String()); err != nil {
		return fail("invalid_target_connection")
	}
	// All clients must select the same database and principal. pgx accepts
	// query overrides that native backup tools and pgloader do not interpret
	// consistently; require these fields in the URL authority and path instead.
	for _, option := range []string{"dbname", "database", "host", "hostaddr", "port", "user", "password", "service", "servicefile", "options"} {
		if _, exists := targetURL.Query()[option]; exists {
			return fail("target_connection_overrides_not_supported")
		}
	}
	if searchPath := targetURL.Query().Get("search_path"); searchPath != "" && searchPath != "public" {
		return fail("target_schema_must_be_public")
	}
	clients := []string{"pg_dump"}
	if *source != "postgres" {
		clients = append(clients, "pgloader")
	}
	if *source == "sqlite" {
		clients = append(clients, "sqlite3")
	}
	if *source == "mysql" {
		clients = append(clients, "mysql", "mysqldump")
	}
	for _, client := range clients {
		if _, err := exec.LookPath(client); err != nil {
			return fail("missing_client_" + client)
		}
	}
	legacy, err := prepareSource(*source, *sqlitePath, os.Getenv(*mysqlEnv))
	if err != nil {
		return fail("invalid_source")
	}
	common.DebugEnabled = false
	if err := model.InitSettlementCurrency(); err != nil {
		return fail("invalid_settlement_currency")
	}
	db, _, err := model.OpenDSN(*targetEnv, targetURL.String(), false)
	if err != nil {
		return fail("target_connection_failed")
	}
	db = db.Session(&gorm.Session{Logger: logger.Discard}).WithContext(ctx)
	sqlDB, err := db.DB()
	if err != nil {
		return fail("target_connection_failed")
	}
	defer sqlDB.Close()
	var schema string
	if err := db.Raw("SELECT current_schema()").Scan(&schema).Error; err != nil || schema != "public" {
		return fail("target_schema_must_be_public")
	}
	var version int
	if err := db.Raw("SHOW server_version_num").Scan(&version).Error; err != nil || version < 140000 {
		return fail("postgresql_14_required")
	}
	lock, err := sqlDB.Conn(ctx)
	if err != nil {
		return fail("target_lock_failed")
	}
	defer lock.Close()
	var locked bool
	if err := lock.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(45002445)").Scan(&locked); err != nil || !locked {
		return fail("migration_already_running")
	}
	defer lock.ExecContext(context.Background(), "SELECT pg_advisory_unlock(45002445)")
	if err := os.MkdirAll(*backupRoot, 0o700); err != nil {
		return fail("backup_directory_failed")
	}
	backupDir, err := os.MkdirTemp(*backupRoot, "legacy-")
	if err != nil {
		return fail("backup_directory_failed")
	}
	report.BackupDirectory, err = filepath.Abs(backupDir)
	if err != nil {
		return fail("backup_directory_failed")
	}
	if err := backupPostgres(ctx, targetURL, filepath.Join(backupDir, "target.dump")); err != nil {
		return fail("target_backup_failed")
	}
	if *source != "postgres" {
		if err := legacy.backup(ctx, backupDir); err != nil {
			return fail("source_backup_failed")
		}
		counts, err := legacy.tableCounts(ctx)
		if err != nil {
			return fail("source_inventory_failed")
		}
		report.Tables = counts
	}
	// Persist the backup directory entries before changing the target database.
	for _, path := range []string{backupDir, filepath.Dir(backupDir)} {
		directory, err := os.Open(path)
		if err != nil {
			return fail("backup_sync_failed")
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil || closeErr != nil {
			return fail("backup_sync_failed")
		}
	}
	if *source != "postgres" {
		if err := importSource(ctx, db, targetURL, legacy, report.Tables, backupDir); err != nil {
			return fail(err.Error())
		}
	}
	previousDB := model.DB
	model.DB = db
	defer func() { model.DB = previousDB }()
	report.Gateway, err = model.MigrateLegacyGateway(ctx)
	if err != nil {
		return fail("gateway_migration_blocked")
	}
	report.Status = "completed"
	if !writeReport(report, stdout) {
		return 1
	}
	return 0
}

func writeReport(report migrationReport, output io.Writer) bool {
	encoded, err := common.Marshal(report)
	if err != nil {
		return false
	}
	encoded = append(encoded, '\n')
	if report.BackupDirectory != "" {
		file, err := os.OpenFile(filepath.Join(report.BackupDirectory, "report.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return false
		}
		_, writeErr := file.Write(encoded)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return false
		}
		directory, err := os.Open(report.BackupDirectory)
		if err != nil {
			return false
		}
		syncErr = directory.Sync()
		closeErr = directory.Close()
		if syncErr != nil || closeErr != nil {
			return false
		}
	}
	_, err = output.Write(encoded)
	return err == nil
}

func backupPostgres(ctx context.Context, target *url.URL, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	command := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-password")
	command.Env = postgresEnvironment(target)
	command.Stdout = file
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return err
	}
	return file.Sync()
}

func postgresEnvironment(target *url.URL) []string {
	environment := os.Environ()
	user := ""
	password := ""
	if target.User != nil {
		user = target.User.Username()
		password, _ = target.User.Password()
	}
	port := target.Port()
	if port == "" {
		port = "5432"
	}
	environment = append(environment, "PGHOST="+target.Hostname(), "PGPORT="+port, "PGDATABASE="+strings.TrimPrefix(target.Path, "/"))
	if user != "" {
		environment = append(environment, "PGUSER="+user)
	}
	if password != "" {
		environment = append(environment, "PGPASSWORD="+password)
	}
	for option, variable := range map[string]string{"sslmode": "PGSSLMODE", "sslcert": "PGSSLCERT", "sslkey": "PGSSLKEY", "sslrootcert": "PGSSLROOTCERT", "sslcrl": "PGSSLCRL", "connect_timeout": "PGCONNECT_TIMEOUT"} {
		if value := target.Query().Get(option); value != "" {
			environment = append(environment, variable+"="+value)
		}
	}
	return environment
}

func importSource(ctx context.Context, db *gorm.DB, target *url.URL, source *legacySource, counts map[string]int64, directory string) error {
	var names []string
	if err := db.Raw("SELECT tablename FROM pg_tables WHERE schemaname = 'public'").Scan(&names).Error; err != nil {
		return errors.New("target_inventory_failed")
	}
	var state sourceImport
	stateTable := "gateway_migration.source_imports"
	if db.Migrator().HasTable(stateTable) {
		err := db.Table(stateTable).First(&state, 1).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("import_state_failed")
		}
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(source.identity)))
	if state.ID != 0 && state.Fingerprint != fingerprint {
		return errors.New("different_source_already_imported")
	}
	if state.Phase == "imported" {
		return nil
	}
	loaderSource := source
	var partialIndexes []legacySQLitePartialIndex
	if source.kind == "sqlite" {
		var err error
		loaderSource, partialIndexes, err = source.sqliteLoaderCopy(ctx, directory)
		if err != nil {
			return errors.New("source_partial_index_unsupported")
		}
		if loaderSource != source {
			defer os.Remove(loaderSource.path)
		}
	}
	if len(names) != 0 {
		return errors.New("cross_database_target_must_be_empty")
	}
	if err := db.Exec("CREATE SCHEMA IF NOT EXISTS gateway_migration").Error; err != nil {
		return errors.New("import_state_failed")
	}
	if err := db.Table(stateTable).AutoMigrate(&sourceImport{}); err != nil {
		return errors.New("import_state_failed")
	}
	if state.Phase != "loaded" {
		schema := "legacy_import_" + fmt.Sprintf("%x", rand.Text()[:12])
		if err := db.Exec("CREATE SCHEMA " + quoteIdentifier(schema)).Error; err != nil {
			return errors.New("staging_schema_failed")
		}
		state = sourceImport{ID: 1, Fingerprint: fingerprint, Schema: schema, Phase: "loading"}
		if err := db.Table(stateTable).Save(&state).Error; err != nil {
			return errors.New("import_state_failed")
		}
		connection := *target
		query := url.Values{}
		if sslMode := target.Query().Get("sslmode"); sslMode != "" {
			query.Set("sslmode", sslMode)
		}
		connection.RawQuery = query.Encode()
		configuration := "LOAD DATABASE FROM " + loaderSource.url + " INTO " + connection.String() + " WITH include no drop, no truncate, create tables, create indexes, drop indexes, reset sequences SET PostgreSQL PARAMETERS search_path to '" + schema + ", public'"
		if source.kind == "mysql" {
			configuration += " ALTER SCHEMA '" + strings.ReplaceAll(source.database, "'", "''") + "' RENAME TO '" + schema + "'"
		}
		if source.kind == "sqlite" {
			casts, err := source.sqliteBooleanCasts(ctx)
			if err != nil {
				return errors.New("source_boolean_schema_invalid")
			}
			if len(casts) > 0 {
				configuration += " CAST " + strings.Join(casts, ", ")
			}
		}
		configuration += ";\n"
		loadPath := filepath.Join(directory, "source.load")
		file, err := os.OpenFile(loadPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return errors.New("loader_configuration_failed")
		}
		_, writeErr := io.WriteString(file, configuration)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(loadPath)
			return errors.New("loader_configuration_failed")
		}
		command := exec.CommandContext(ctx, "pgloader", "--on-error-stop", "--logfile", os.DevNull, "--root-dir", directory, loadPath)
		var diagnostic bytes.Buffer
		command.Stdout = &diagnostic
		command.Stderr = &diagnostic
		loadErr := command.Run()
		removeErr := os.Remove(loadPath)
		if loadErr != nil || removeErr != nil || bytes.Contains(diagnostic.Bytes(), []byte("ERROR")) || bytes.Contains(diagnostic.Bytes(), []byte("FATAL")) {
			// Never include loader output: it may contain connection credentials
			// or values from rejected rows, even when its exit status is zero.
			return errors.New("source_load_failed")
		}
		state.Phase = "loaded"
		if err := db.Table(stateTable).Save(&state).Error; err != nil {
			return errors.New("import_state_failed")
		}
	}
	for table, expected := range counts {
		var actual int64
		if err := db.Raw("SELECT count(*) FROM " + quoteIdentifier(state.Schema) + "." + quoteIdentifier(table)).Scan(&actual).Error; err != nil {
			return errors.New("import_table_read_failed")
		}
		if actual != expected {
			return errors.New("import_count_mismatch")
		}
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if source.kind == "sqlite" {
			for _, index := range partialIndexes {
				unique := ""
				if index.unique {
					unique = "UNIQUE "
				}
				if err := tx.Exec("CREATE " + unique + "INDEX " + quoteIdentifier(index.name) + " ON " + quoteIdentifier(state.Schema) + "." + quoteIdentifier(index.table) + " (" + strings.Join(index.columns, ",") + ") WHERE " + index.predicate).Error; err != nil {
					return errors.New("source_partial_index_restore_failed")
				}
			}
		}
		if err := restoreSourceIdentity(ctx, tx, source, state.Schema); err != nil {
			return errors.New("source_identity_restore_failed")
		}
		var tables []string
		if err := tx.Raw("SELECT tablename FROM pg_tables WHERE schemaname = ? ORDER BY tablename", state.Schema).Scan(&tables).Error; err != nil || len(tables) != len(counts) {
			return errors.New("import_table_mismatch")
		}
		for _, table := range tables {
			if err := tx.Exec("ALTER TABLE " + quoteIdentifier(state.Schema) + "." + quoteIdentifier(table) + " SET SCHEMA public").Error; err != nil {
				return errors.New("import_promotion_failed")
			}
		}
		state.Phase = "imported"
		if err := tx.Table(stateTable).Save(&state).Error; err != nil {
			return errors.New("import_state_failed")
		}
		return nil
	}); err != nil {
		return errors.New("import_promotion_failed")
	}
	return nil
}

// Preserve the source high-water mark as well as existing rows: deleted IDs
// must not be reused. pgloader resets sequences from MAX(id) and can omit the
// generator on empty SQLite tables, so both need restoration before promotion.
func restoreSourceIdentity(ctx context.Context, db *gorm.DB, source *legacySource, targetSchema string) error {
	query := "SELECT m.name AS table_name,p.name AS column_name FROM sqlite_master m,pragma_table_info(m.name) p WHERE m.type='table' AND upper(m.sql) LIKE '%AUTOINCREMENT%' AND p.pk=1 AND lower(p.type)='integer'"
	if source.kind == "mysql" {
		query = "SELECT JSON_OBJECT('table_name',c.TABLE_NAME,'column_name',c.COLUMN_NAME,'last_value',CAST(COALESCE(t.AUTO_INCREMENT,1)-1 AS CHAR)) FROM information_schema.COLUMNS c JOIN information_schema.TABLES t ON t.TABLE_SCHEMA=c.TABLE_SCHEMA AND t.TABLE_NAME=c.TABLE_NAME WHERE c.TABLE_SCHEMA=DATABASE() AND c.EXTRA LIKE '%auto_increment%'"
	}
	columns, err := source.query(ctx, query)
	if err != nil {
		return err
	}
	for _, column := range columns {
		table, tableOK := column["table_name"].(string)
		name, nameOK := column["column_name"].(string)
		if !tableOK || !nameOK {
			return errors.New("invalid source identity")
		}
		lastValue := "0"
		if source.kind == "mysql" {
			var ok bool
			lastValue, ok = column["last_value"].(string)
			if !ok {
				return errors.New("invalid source identity high-water mark")
			}
		} else {
			sequence, err := source.query(ctx, "SELECT CAST(seq AS TEXT) AS last_value FROM sqlite_sequence WHERE name='"+strings.ReplaceAll(table, "'", "''")+"'")
			if err != nil || len(sequence) > 1 {
				return errors.New("invalid source identity high-water mark")
			}
			if len(sequence) == 1 {
				var ok bool
				lastValue, ok = sequence[0]["last_value"].(string)
				if !ok {
					return errors.New("invalid source identity high-water mark")
				}
			}
		}
		highWater, err := strconv.ParseInt(lastValue, 10, 64)
		if err != nil {
			return errors.New("invalid source identity high-water mark")
		}
		var existing struct {
			ColumnDefault *string
			IsIdentity    string
		}
		if err := db.Raw("SELECT column_default,is_identity FROM information_schema.columns WHERE table_schema=? AND table_name=? AND column_name=?", targetSchema, table, name).Scan(&existing).Error; err != nil {
			return err
		}
		if existing.IsIdentity == "" {
			return errors.New("missing source identity column")
		}
		qualified := quoteIdentifier(targetSchema) + "." + quoteIdentifier(table)
		if existing.ColumnDefault == nil && existing.IsIdentity != "YES" {
			if err := db.Exec("ALTER TABLE " + qualified + " ALTER COLUMN " + quoteIdentifier(name) + " ADD GENERATED BY DEFAULT AS IDENTITY").Error; err != nil {
				return err
			}
		}
		if err := db.Exec("SELECT setval(pg_get_serial_sequence(?,?), GREATEST(COALESCE(MAX("+quoteIdentifier(name)+"),0),?,1), GREATEST(COALESCE(MAX("+quoteIdentifier(name)+"),0),?)>0) FROM "+qualified, qualified, name, highWater, highWater).Error; err != nil {
			return err
		}
	}
	return nil
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
