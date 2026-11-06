package model

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"gorm.io/driver/clickhouse"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// jsonScanBytes 归一化 json 列的驱动返回值:不同驱动/协议模式下同一列可能
// 以 []byte 或 string 返回,静默丢弃 string 会导致字段被清零而不报错。
func jsonScanBytes(value any) []byte {
	switch v := value.(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

// MigrateLogDB creates or updates the log tables on LOG_DB for the current log
// database type.
func MigrateLogDB() error {
	return migrateLOGDB()
}

var DB *gorm.DB

var LOG_DB *gorm.DB

func createRootAccountIfNeed() error {
	var user User
	//if user.Status != common.UserStatusEnabled {
	if err := DB.First(&user).Error; err != nil {
		common.SysLog(common.LogText("no user exists, create a root user for you: username is root, password is 123456"))
		hashedPassword, err := common.Password2Hash("123456")
		if err != nil {
			return err
		}
		rootUser := User{
			Username:    "root",
			Password:    hashedPassword,
			Role:        common.RoleRootUser,
			Status:      common.UserStatusEnabled,
			DisplayName: "Root User",
			AccessToken: nil,
			Quota:       100000000,
		}
		DB.Create(&rootUser)
	}
	return nil
}

func CheckSetup() {
	setup := GetSetup()
	if setup == nil {
		// No setup record exists, check if we have a root user
		if RootUserExists() {
			common.SysLog(common.LogText("system is not initialized, but root user exists"))
			// Create setup record
			newSetup := Setup{
				Version:       common.Version,
				InitializedAt: time.Now().Unix(),
			}
			err := DB.Create(&newSetup).Error
			if err != nil {
				common.SysLog(common.LogText("failed to create setup record: %s", err.Error()))
			}
			constant.Setup = true
		} else {
			common.SysLog(common.LogText("system is not initialized and no root user exists"))
			constant.Setup = false
		}
	} else {
		// Setup record exists, system is initialized
		common.SysLog(common.LogText("system is already initialized at: %s", time.Unix(setup.InitializedAt, 0).String()))
		constant.Setup = true
	}
}

func isClickHouseDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "clickhouse://") ||
		strings.HasPrefix(dsn, "tcp://") ||
		strings.HasPrefix(dsn, "http://") ||
		strings.HasPrefix(dsn, "https://")
}

func normalizeClickHouseDSN(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "https" {
		return dsn
	}
	query := parsed.Query()
	if _, ok := query["secure"]; !ok {
		query.Set("secure", "true")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

func isPostgresDSN(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// OpenDSN opens the database described by dsn with the same driver settings the
// gateway uses at startup. envName only labels error messages. The primary
// database must be PostgreSQL; the log database may be PostgreSQL or
// ClickHouse.
func OpenDSN(envName string, dsn string, isLog bool) (*gorm.DB, common.DatabaseType, error) {
	if dsn == "" {
		return nil, "", fmt.Errorf("%s is not set; set it to a PostgreSQL DSN such as postgres://user:password@host:5432/new-api", envName)
	}
	if isClickHouseDSN(dsn) {
		if !isLog {
			return nil, "", fmt.Errorf("%s does not support ClickHouse; use PostgreSQL for the primary database and LOG_SQL_DSN for ClickHouse logs", envName)
		}
		common.SysLog(common.LogText("using ClickHouse as log database"))
		db, err := gorm.Open(clickhouse.Open(normalizeClickHouseDSN(dsn)), newGormConfig())
		return db, common.DatabaseTypeClickHouse, err
	}
	if !isPostgresDSN(dsn) {
		if isLog {
			return nil, "", fmt.Errorf("%s must be a PostgreSQL (postgres://) or ClickHouse (clickhouse://) DSN; SQLite and MySQL are not supported", envName)
		}
		return nil, "", fmt.Errorf("%s must be a PostgreSQL DSN (postgres:// or postgresql://); SQLite and MySQL are not supported", envName)
	}
	common.SysLog(common.LogText("using PostgreSQL as database"))
	// 同时关闭 pgx 隐式与 GORM 显式预处理语句:命名 prepared statement 与
	// 事务池代理(PgBouncer/Neon/Supabase)不兼容,会触发 FATAL 08P01/42P05。
	db, err := gorm.Open(postgresMigrationDialector{postgres.Dialector{Config: &postgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	}}}, newGormConfig())
	return db, common.DatabaseTypePostgreSQL, err
}

func InitDB() (err error) {
	db, _, err := OpenDSN("SQL_DSN", os.Getenv("SQL_DSN"), false)
	if err == nil {
		if os.Getenv("LOG_SQL_DSN") == "" {
			common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
		}
		if common.DebugEnabled {
			db = db.Debug()
		}
		DB = db
		if err := ensureUserQuotaColumns(DB); err != nil {
			return err
		}
		sqlDB, err := DB.DB()
		if err != nil {
			return err
		}
		sqlDB.SetMaxIdleConns(common.GetEnvOrDefault("SQL_MAX_IDLE_CONNS", 100))
		sqlDB.SetMaxOpenConns(common.GetEnvOrDefault("SQL_MAX_OPEN_CONNS", 1000))
		sqlDB.SetConnMaxLifetime(time.Second * time.Duration(common.GetEnvOrDefault("SQL_MAX_LIFETIME", 60)))

		if !common.IsMasterNode {
			// Only the master node migrates. A node that cannot read the deadline
			// keeps rejecting legacy access tokens instead of refusing to start.
			if err := EnsureLegacyAccessTokenRetireAt(common.GetTimestamp()); err != nil {
				common.SysError(common.LogText("initialize legacy access token deadline: %s", err.Error()))
			}
			return nil
		}
		common.SysLog(common.LogText("database migration started"))
		err = migrateDB()
		return err
	} else {
		common.FatalLog(err)
	}
	return err
}

func InitLogDB() (err error) {
	if os.Getenv("LOG_SQL_DSN") == "" {
		LOG_DB = DB
		common.SetLogDatabaseType(common.DatabaseTypePostgreSQL)
		if common.IsMasterNode {
			return MigrateAuditLogs()
		}
		return
	}
	db, dbType, err := OpenDSN("LOG_SQL_DSN", os.Getenv("LOG_SQL_DSN"), true)
	if err == nil {
		common.SetLogDatabaseType(dbType)
		if common.DebugEnabled {
			db = db.Debug()
		}
		LOG_DB = db
		sqlDB, err := LOG_DB.DB()
		if err != nil {
			return err
		}
		sqlDB.SetMaxIdleConns(common.GetEnvOrDefault("SQL_MAX_IDLE_CONNS", 100))
		sqlDB.SetMaxOpenConns(common.GetEnvOrDefault("SQL_MAX_OPEN_CONNS", 1000))
		sqlDB.SetConnMaxLifetime(time.Second * time.Duration(common.GetEnvOrDefault("SQL_MAX_LIFETIME", 60)))

		if !common.IsMasterNode {
			return nil
		}
		common.SysLog(common.LogText("database migration started"))
		err = migrateLOGDB()
		return err
	} else {
		common.FatalLog(err)
	}
	return err
}

var userQuotaColumns = []string{"quota", "used_quota", "aff_quota", "aff_history"}

// ensureUserQuotaColumns rejects a legacy 32-bit wallet schema before any
// migrations run. The 64-bit-only build intentionally does not auto-upgrade
// an existing wallet; operators must migrate it explicitly before starting.
func ensureUserQuotaColumns(db *gorm.DB) error {
	if common.GetEnvOrDefaultBool("SKIP_64BIT_QUOTA_SCHEMA_CHECK", false) {
		common.SysLog(common.LogText("SKIP_64BIT_QUOTA_SCHEMA_CHECK=true; skipping user quota schema check"))
		return nil
	}
	if db == nil {
		return nil
	}
	if !db.Migrator().HasTable(&User{}) {
		return nil
	}
	columnTypes, err := db.Migrator().ColumnTypes(&User{})
	if err != nil {
		return fmt.Errorf("failed to inspect users schema: %w", err)
	}
	for _, expected := range userQuotaColumns {
		for _, actual := range columnTypes {
			if !strings.EqualFold(actual.Name(), expected) {
				continue
			}
			dataType := strings.ToLower(strings.TrimSpace(actual.DatabaseTypeName()))
			if dataType != "bigint" && dataType != "int8" {
				return fmt.Errorf("users.%s uses %s; 32-bit is not supported", expected, dataType)
			}
		}
	}
	return nil
}

func migrateDB() error {
	if err := migrateTokenKeyUniqueness(DB); err != nil {
		return err
	}
	if err := migratePrefillGroupUniqueness(DB); err != nil {
		return err
	}
	// Migrate price_amount column from float/double to decimal for existing tables
	migrateSubscriptionPlanPriceAmount()
	// Migrate model_limits column from varchar to text for existing tables
	if err := migrateTokenModelLimitsToText(); err != nil {
		return err
	}
	if err := migrateOptionPrimaryKey(DB); err != nil {
		common.SysError(common.LogText("failed to migrate options primary key: %s", err.Error()))
	}

	err := DB.AutoMigrate(
		&Channel{},
		&Token{},
		&User{},
		&UserSession{},
		&AuthFlow{},
		&ExternalIdentityClaim{},
		&PasskeyCredential{},
		&Option{},
		&LoginEncryptionKey{},
		&Redemption{},
		&Ability{},
		&Log{},
		&Midjourney{},
		&TopUp{},
		&QuotaData{},
		&Task{},
		&TaskPlugin{},
		&Model{},
		&Vendor{},
		&PrefillGroup{},
		&Setup{},
		&TwoFA{},
		&TwoFABackupCode{},
		&Checkin{},
		&SubscriptionOrder{},
		&UserSubscription{},
		&SubscriptionPreConsumeRecord{},
		&CustomOAuthProvider{},
		&UserOAuthBinding{},
		&PerfMetric{},
		&SystemInstance{},
		&SystemTask{},
		&SystemTaskLock{},
		&CasbinRule{},
		&AuthzRole{},
		&UserAccessToken{},
	)
	if err != nil {
		return err
	}
	if err := InitializeUserAuthVersions(); err != nil {
		return err
	}
	if err := EnsureLegacyAccessTokenRetireAt(common.GetTimestamp()); err != nil {
		return fmt.Errorf("initialize legacy access token deadline: %w", err)
	}
	if err := InitializeExternalIdentityClaims(); err != nil {
		return err
	}
	return DB.AutoMigrate(&SubscriptionPlan{})
}

func migrateLOGDB() error {
	if err := MigrateAuditLogs(); err != nil {
		return err
	}
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		return migrateClickHouseLogDB()
	}
	return LOG_DB.AutoMigrate(&Log{})
}

func migrateClickHouseLogDB() error {
	ttlDays := clickHouseLogTTLDays()
	if err := LOG_DB.Exec(clickHouseLogCreateTableSQL(ttlDays)).Error; err != nil {
		return err
	}
	return syncClickHouseLogTTL(ttlDays)
}

func clickHouseLogTTLDays() int {
	ttlDays := common.GetEnvOrDefault("LOG_SQL_CLICKHOUSE_TTL_DAYS", 0)
	if ttlDays < 0 {
		return 0
	}
	return ttlDays
}

func clickHouseLogTTLExpression(ttlDays int) string {
	if ttlDays <= 0 {
		return ""
	}
	return fmt.Sprintf("toDateTime(created_at) + INTERVAL %d DAY DELETE", ttlDays)
}

func clickHouseLogTTLClause(ttlDays int) string {
	expression := clickHouseLogTTLExpression(ttlDays)
	if expression == "" {
		return ""
	}
	return "\nTTL " + expression
}

func clickHouseLogCreateTableSQL(ttlDays int) string {
	return fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS logs (
	id Int64 DEFAULT 0,
	user_id Int32 DEFAULT 0,
	created_at Int64 DEFAULT 0,
	type Int32 DEFAULT 0,
	content String DEFAULT '',
	username String DEFAULT '',
	token_name String DEFAULT '',
	model_name String DEFAULT '',
	quota Int32 DEFAULT 0,
	prompt_tokens Int32 DEFAULT 0,
	completion_tokens Int32 DEFAULT 0,
	use_time Int32 DEFAULT 0,
	is_stream UInt8 DEFAULT 0,
	channel_id Int32 DEFAULT 0,
	token_id Int32 DEFAULT 0,
	`+"`group`"+` String DEFAULT '',
	ip String DEFAULT '',
	request_id String DEFAULT '',
	upstream_request_id String DEFAULT '',
	other String DEFAULT ''
)
ENGINE = MergeTree()
PARTITION BY toYYYYMM(toDateTime(created_at))
ORDER BY (created_at, request_id)%s`, clickHouseLogTTLClause(ttlDays))
}

func syncClickHouseLogTTL(ttlDays int) error {
	expression := clickHouseLogTTLExpression(ttlDays)
	if expression != "" {
		return LOG_DB.Exec("ALTER TABLE logs MODIFY TTL " + expression).Error
	}

	hasTTL, err := clickHouseLogTableHasTTL()
	if err != nil {
		return err
	}
	if !hasTTL {
		return nil
	}
	return LOG_DB.Exec("ALTER TABLE logs REMOVE TTL").Error
}

func clickHouseLogTableHasTTL() (bool, error) {
	var createTableSQL string
	if err := LOG_DB.Raw("SHOW CREATE TABLE logs").Scan(&createTableSQL).Error; err != nil {
		return false, err
	}
	return clickHouseCreateTableHasTTL(createTableSQL), nil
}

func clickHouseCreateTableHasTTL(createTableSQL string) bool {
	upperSQL := strings.ToUpper(createTableSQL)
	return strings.Contains(upperSQL, "\nTTL ") || strings.Contains(upperSQL, " TTL ")
}

// migrateTokenModelLimitsToText migrates model_limits column from varchar(1024) to text
// This is safe to run multiple times - it checks the column type first
func migrateTokenModelLimitsToText() error {
	tableName := "tokens"
	columnName := "model_limits"

	if !DB.Migrator().HasTable(tableName) {
		return nil
	}

	if !DB.Migrator().HasColumn(&Token{}, columnName) {
		return nil
	}

	var dataType string
	if err := DB.Raw(`SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		tableName, columnName).Scan(&dataType).Error; err != nil {
		common.SysLog(common.LogText("Warning: failed to query metadata for %s.%s: %v", tableName, columnName, err))
	} else if dataType == "text" {
		return nil
	}
	if err := DB.Exec(fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE text`, tableName, columnName)).Error; err != nil {
		return fmt.Errorf("failed to migrate %s.%s to text: %w", tableName, columnName, err)
	}
	common.SysLog(common.LogText("Successfully migrated %s.%s to text", tableName, columnName))
	return nil
}

// migrateSubscriptionPlanPriceAmount migrates price_amount column from float/double to decimal(10,6)
// This is safe to run multiple times - it checks the column type first
func migrateSubscriptionPlanPriceAmount() {
	tableName := "subscription_plans"
	columnName := "price_amount"

	if !DB.Migrator().HasTable(tableName) {
		return
	}

	if !DB.Migrator().HasColumn(&SubscriptionPlan{}, columnName) {
		return
	}

	var dataType string
	if err := DB.Raw(`SELECT data_type FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
		tableName, columnName).Scan(&dataType).Error; err != nil {
		common.SysLog(common.LogText("Warning: failed to query metadata for %s.%s: %v", tableName, columnName, err))
	} else if dataType == "numeric" {
		return
	}
	alterSQL := fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s TYPE decimal(10,6) USING %s::decimal(10,6)`,
		tableName, columnName, columnName)
	if err := DB.Exec(alterSQL).Error; err != nil {
		common.SysLog(common.LogText("Warning: failed to migrate %s.%s to decimal: %v", tableName, columnName, err))
		return
	}
	common.SysLog(common.LogText("Successfully migrated %s.%s to decimal(10,6)", tableName, columnName))
}

func closeDB(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	err = sqlDB.Close()
	return err
}

func CloseDB() error {
	if LOG_DB != DB {
		err := closeDB(LOG_DB)
		if err != nil {
			return err
		}
	}
	return closeDB(DB)
}

var (
	lastPingTime time.Time
	pingMutex    sync.Mutex
)

func PingDB() error {
	pingMutex.Lock()
	defer pingMutex.Unlock()

	if time.Since(lastPingTime) < time.Second*10 {
		return nil
	}

	sqlDB, err := DB.DB()
	if err != nil {
		log.Print(common.LogText("Error getting sql.DB from GORM: %v", err))
		return err
	}

	err = sqlDB.Ping()
	if err != nil {
		log.Print(common.LogText("Error pinging DB: %v", err))
		return err
	}

	lastPingTime = time.Now()
	common.SysLog(common.LogText("Database pinged successfully"))
	return nil
}
