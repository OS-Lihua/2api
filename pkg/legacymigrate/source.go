package legacymigrate

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// The released SQLite schema stores these booleans as NUMERIC. Keep this
// compatibility list independent of future removals from the runtime models;
// arbitrary NUMERIC columns, including historical money, retain their types.
var legacySQLiteBooleanColumns = map[string]bool{
	"abilities.enabled":                        true,
	"audit_logs.success":                       true,
	"authz_roles.built_in":                     true,
	"authz_roles.enabled":                      true,
	"custom_oauth_providers.enabled":           true,
	"logs.is_stream":                           true,
	"passkey_credentials.clone_warning":        true,
	"passkey_credentials.user_present":         true,
	"passkey_credentials.user_verified":        true,
	"passkey_credentials.backup_eligible":      true,
	"passkey_credentials.backup_state":         true,
	"subscription_plans.enabled":               true,
	"subscription_plans.allow_balance_pay":     true,
	"subscription_plans.allow_wallet_overflow": true,
	"task_plugins.enabled":                     true,
	"task_plugins.active":                      true,
	"tokens.unlimited_quota":                   true,
	"tokens.model_limits_enabled":              true,
	"tokens.cross_group_retry":                 true,
	"tokens.allow_user_quota_query":            true,
	"two_fa_backup_codes.is_used":              true,
	"two_fas.is_enabled":                       true,
	"user_subscriptions.allow_wallet_overflow": true,
}

func (source *legacySource) sqliteBooleanCasts(ctx context.Context) ([]string, error) {
	rows, err := source.query(ctx, "SELECT m.name AS table_name,p.name AS column_name FROM sqlite_master m,pragma_table_info(m.name) p WHERE m.type='table' AND lower(p.type)='numeric'")
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, row := range rows {
		table, tableOK := row["table_name"].(string)
		column, columnOK := row["column_name"].(string)
		if !tableOK || !columnOK {
			return nil, errors.New("invalid boolean schema")
		}
		present[table+"."+column] = true
	}
	var casts []string
	for _, name := range slices.Sorted(maps.Keys(legacySQLiteBooleanColumns)) {
		if !present[name] {
			continue
		}
		table, column, _ := strings.Cut(name, ".")
		invalid, err := source.query(ctx, "SELECT CAST(count(*) AS TEXT) AS count FROM "+quoteIdentifier(table)+" WHERE "+quoteIdentifier(column)+" IS NOT NULL AND "+quoteIdentifier(column)+" NOT IN (0,1)")
		if err != nil || len(invalid) != 1 || invalid[0]["count"] != "0" {
			return nil, errors.New("invalid legacy boolean")
		}
		casts = append(casts, "column "+name+" to boolean drop typemod using sql-server-bit-to-boolean")
	}
	return casts, nil
}

type legacySQLitePartialIndex struct {
	name      string
	table     string
	columns   []string
	unique    bool
	predicate string
}

// pgloader ignores SQLite partial-index predicates. Load a separate copy without
// these indexes, then restore their original uniqueness inside promotion. The
// released schema uses IS NULL predicates; reject other syntax rather than
// silently widening a constraint or interpreting arbitrary SQLite expressions.
func (source *legacySource) sqliteLoaderCopy(ctx context.Context, directory string) (*legacySource, []legacySQLitePartialIndex, error) {
	rows, err := source.query(ctx, `SELECT m.name AS table_name,i.name AS index_name,s.sql AS definition,CAST(i."unique" AS TEXT) AS is_unique FROM sqlite_master m,pragma_index_list(m.name) i JOIN sqlite_master s ON s.name=i.name WHERE m.type='table' AND i.partial=1`)
	if err != nil || len(rows) == 0 {
		return source, nil, err
	}
	predicate := regexp.MustCompile(`(?i)\bWHERE\s+([a-z_][a-z0-9_]*)\s+IS\s+(NOT\s+)?NULL\s*;?\s*$`)
	indexes := make([]legacySQLitePartialIndex, 0, len(rows))
	for _, row := range rows {
		table, tableOK := row["table_name"].(string)
		name, nameOK := row["index_name"].(string)
		definition, definitionOK := row["definition"].(string)
		unique, uniqueOK := row["is_unique"].(string)
		if !tableOK || !nameOK || !definitionOK || !uniqueOK {
			return nil, nil, errors.New("invalid SQLite partial index")
		}
		condition := predicate.FindStringSubmatch(definition)
		if len(condition) != 3 {
			return nil, nil, errors.New("unsupported SQLite partial index")
		}
		index := legacySQLitePartialIndex{name: name, table: table, unique: unique == "1", predicate: quoteIdentifier(condition[1]) + " IS "}
		if condition[2] != "" {
			index.predicate += "NOT "
		}
		index.predicate += "NULL"
		columns, err := source.query(ctx, "SELECT name FROM pragma_index_info('"+strings.ReplaceAll(name, "'", "''")+"') ORDER BY seqno")
		if err != nil || len(columns) == 0 {
			return nil, nil, errors.New("invalid SQLite partial index columns")
		}
		for _, column := range columns {
			value, ok := column["name"].(string)
			if !ok || value == "" {
				return nil, nil, errors.New("unsupported SQLite partial index expression")
			}
			index.columns = append(index.columns, quoteIdentifier(value))
		}
		indexes = append(indexes, index)
	}
	copy := *source
	copy.path = filepath.Join(directory, "source-loader.sqlite")
	file, err := os.OpenFile(copy.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		os.Remove(copy.path)
		return nil, nil, err
	}
	backup := exec.CommandContext(ctx, "sqlite3", source.path, ".backup "+strconv.Quote(copy.path))
	backup.Stdout, backup.Stderr = io.Discard, io.Discard
	if err := backup.Run(); err != nil {
		os.Remove(copy.path)
		return nil, nil, err
	}
	var drop strings.Builder
	for _, index := range indexes {
		drop.WriteString("DROP INDEX " + quoteIdentifier(index.name) + ";")
	}
	command := exec.CommandContext(ctx, "sqlite3", copy.path, drop.String())
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		os.Remove(copy.path)
		return nil, nil, err
	}
	copy.url = (&url.URL{Scheme: "sqlite", Path: copy.path}).String()
	return &copy, indexes, nil
}

type legacySource struct {
	kind     string
	identity string
	url      string
	path     string
	database string
	mysql    *url.URL
}

func prepareSource(kind, sqlitePath, mysqlURL string) (*legacySource, error) {
	source := &legacySource{kind: kind}
	switch kind {
	case "postgres":
		return source, nil
	case "sqlite":
		path, err := filepath.Abs(sqlitePath)
		if err != nil || sqlitePath == "" {
			return nil, errors.New("invalid SQLite file")
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("invalid SQLite file")
		}
		source.path = path
		source.identity = "sqlite:" + path
	case "mysql":
		parsed, err := url.Parse(mysqlURL)
		if err != nil || parsed.Scheme != "mysql" || parsed.Hostname() == "" || parsed.User == nil || parsed.User.Username() == "" {
			return nil, errors.New("invalid MySQL connection")
		}
		source.database = strings.TrimPrefix(parsed.Path, "/")
		if source.database == "" || strings.ContainsAny(source.database, "/\x00\n\r") || parsed.RawQuery != "" {
			return nil, errors.New("invalid MySQL database")
		}
		source.mysql = parsed
		source.url = parsed.String()
		source.identity = "mysql:" + parsed.Host + "/" + source.database
	default:
		return nil, errors.New("unsupported source")
	}
	return source, nil
}

func (source *legacySource) backup(ctx context.Context, directory string) error {
	if source.kind == "sqlite" {
		backup := filepath.Join(directory, "source.sqlite")
		command := exec.CommandContext(ctx, "sqlite3", source.path, ".backup "+strconv.Quote(backup))
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		if err := command.Run(); err != nil {
			return err
		}
		// The standalone backup must not depend on WAL sidecars when pgloader
		// opens it read-only. This changes only the private backup.
		normalize := exec.CommandContext(ctx, "sqlite3", backup, "PRAGMA journal_mode=DELETE;")
		normalize.Stdout, normalize.Stderr = io.Discard, io.Discard
		if err := normalize.Run(); err != nil {
			return err
		}
		if err := os.Chmod(backup, 0o600); err != nil {
			return err
		}
		file, err := os.OpenFile(backup, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := file.Sync(); err != nil {
			return err
		}
		source.path = backup
		source.url = (&url.URL{Scheme: "sqlite", Path: backup}).String()
		return nil
	}
	file, err := os.OpenFile(filepath.Join(directory, "source.mysql.sql"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	command := source.mysqlCommand(ctx, "mysqldump", "--single-transaction", "--skip-lock-tables", "--routines", "--events", "--triggers", "--hex-blob", "--set-gtid-purged=OFF", "--databases", source.database)
	command.Stdout = file
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return err
	}
	return file.Sync()
}

func (source *legacySource) mysqlCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	port := source.mysql.Port()
	if port == "" {
		port = "3306"
	}
	arguments := []string{"--no-defaults", "--host=" + source.mysql.Hostname(), "--port=" + port, "--user=" + source.mysql.User.Username(), "--default-character-set=utf8mb4"}
	arguments = append(arguments, args...)
	command := exec.CommandContext(ctx, name, arguments...)
	password, _ := source.mysql.User.Password()
	command.Env = append(os.Environ(), "MYSQL_PWD="+password)
	return command
}

func (source *legacySource) query(ctx context.Context, query string) ([]map[string]any, error) {
	var command *exec.Cmd
	if source.kind == "sqlite" {
		command = exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", source.path, query)
	} else {
		command = source.mysqlCommand(ctx, "mysql", "--batch", "--skip-column-names", "--raw", "--database="+source.database)
		command.Stdin = strings.NewReader(query)
	}
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var rows []map[string]any
	if len(bytes.TrimSpace(output)) == 0 {
		return rows, nil
	}
	if source.kind == "sqlite" {
		err = common.UnmarshalUseNumber(output, &rows)
		return rows, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		var row map[string]any
		if err := common.UnmarshalUseNumber(scanner.Bytes(), &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

func (source *legacySource) tableCounts(ctx context.Context) (map[string]int64, error) {
	query := "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"
	if source.kind == "mysql" {
		query = "SELECT JSON_OBJECT('name', TABLE_NAME) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND TABLE_TYPE='BASE TABLE' ORDER BY TABLE_NAME"
	}
	rows, err := source.query(ctx, query)
	if err != nil || len(rows) == 0 {
		return nil, errors.New("source has no tables")
	}
	var parts []string
	for _, row := range rows {
		name, ok := row["name"].(string)
		if !ok || name == "" {
			return nil, errors.New("invalid source table")
		}
		literal := "'" + strings.ReplaceAll(name, "'", "''") + "'"
		if source.kind == "sqlite" {
			parts = append(parts, "SELECT "+literal+" AS name, CAST(count(*) AS TEXT) AS count FROM "+quoteIdentifier(name))
		} else {
			identifier := "`" + strings.ReplaceAll(name, "`", "``") + "`"
			parts = append(parts, "SELECT JSON_OBJECT('name', "+literal+", 'count', CAST(count(*) AS CHAR)) FROM "+identifier)
		}
	}
	rows, err = source.query(ctx, strings.Join(parts, " UNION ALL "))
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int64, len(rows))
	for _, row := range rows {
		name, nameOK := row["name"].(string)
		count, countOK := row["count"].(string)
		if !nameOK || !countOK {
			return nil, errors.New("invalid source count")
		}
		parsed, err := strconv.ParseInt(count, 10, 64)
		if err != nil || parsed < 0 {
			return nil, errors.New("invalid source count")
		}
		counts[name] = parsed
	}
	return counts, nil
}
