package model

import (
	"database/sql"
	"errors"

	"github.com/QuantumNous/new-api/pkg/clock"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrLegacyAbilityRetirement = errors.New("legacy Ability retirement cannot be verified")
	ErrLegacyAbilityOffline    = errors.New("legacy Ability source requires offline migration")
	ErrNativeDatabaseMaster    = errors.New("native database initialization requires a master node")
)

type legacyAbilityDatabaseState uint8

const (
	legacyAbilityEmpty legacyAbilityDatabaseState = iota
	legacyAbilityPresent
	legacyAbilityNative
	legacyAbilityRetired
)

// The proof is independent of both the archive seal and legacy_gateway_v1.
// Native initialization never invents a legacy source, seal, or conversion.
// Legacy retirement binds the complete seal and commits only after source drop.
type legacyAbilityRetirement struct {
	ID             int    `gorm:"primaryKey;autoIncrement:false;check:legacy_ability_retirement_id,id = 1"`
	Version        int    `gorm:"not null;check:legacy_ability_retirement_version,version = 1"`
	Origin         string `gorm:"type:varchar(16);not null"`
	SourceVersion  *int
	SourceRowCount *int64
	SourceSHA256   *string `gorm:"column:source_sha256;type:char(64)"`
	CompletedAt    int64   `gorm:"not null"`
}

// Inspection is read-only and must precede every startup schema write, including
// the slave's access-token deadline initialization. SQL failures are not absence.
func inspectLegacyAbilityStartup(db *gorm.DB) (legacyAbilityDatabaseState, error) {
	var state legacyAbilityDatabaseState
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var err error
		state, err = inspectLegacyAbilityRetirementTx(tx)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return state, ErrLegacyAbilityRetirement
	}
	if state == legacyAbilityPresent {
		return state, ErrLegacyAbilityOffline
	}
	return state, nil
}

func inspectLegacyAbilityRetirementTx(tx *gorm.DB) (legacyAbilityDatabaseState, error) {
	var tables struct{ Source, Proof, Archive, Seal bool }
	if err := tx.Raw(`SELECT to_regclass('abilities') IS NOT NULL AS source,
 to_regclass('legacy_ability_retirements') IS NOT NULL AS proof,
 to_regclass('legacy_ability_sources') IS NOT NULL AS archive,
 to_regclass('legacy_ability_source_manifests') IS NOT NULL AS seal`).Scan(&tables).Error; err != nil {
		return 0, ErrLegacyAbilityRetirement
	}
	if !tables.Proof {
		if tables.Archive != tables.Seal {
			return 0, ErrLegacyAbilityRetirement
		}
		if tables.Archive {
			if !tables.Source {
				return 0, ErrLegacyAbilityRetirement
			}
			if _, err := verifyLegacyAbilitySourceArchiveTx(tx); err != nil {
				return 0, ErrLegacyAbilityRetirement
			}
		}
		if tables.Source {
			return legacyAbilityPresent, nil
		}
		empty, err := emptyNativeApplicationSchema(tx)
		if err != nil || !empty {
			return 0, ErrLegacyAbilityRetirement
		}
		return legacyAbilityEmpty, nil
	}
	if tables.Source {
		return 0, ErrLegacyAbilityRetirement
	}
	if err := tx.Exec(`LOCK TABLE "legacy_ability_retirements" IN ACCESS SHARE MODE`).Error; err != nil {
		return 0, ErrLegacyAbilityRetirement
	}
	var protection int64
	if err := tx.Raw(`SELECT COUNT(*) FROM pg_trigger WHERE NOT tgisinternal AND tgenabled IN ('O','A')
 AND tgrelid=to_regclass('legacy_ability_retirements') AND tgfoid=to_regprocedure('protect_legacy_ability_retirement()')
 AND tgname IN ('legacy_ability_retirement_immutable','legacy_ability_retirement_no_truncate')`).Scan(&protection).Error; err != nil || protection != 2 {
		return 0, ErrLegacyAbilityRetirement
	}
	var proofs []legacyAbilityRetirement
	if err := tx.Limit(2).Find(&proofs).Error; err != nil || len(proofs) != 1 {
		return 0, ErrLegacyAbilityRetirement
	}
	proof := proofs[0]
	if proof.ID != 1 || proof.Version != 1 {
		return 0, ErrLegacyAbilityRetirement
	}
	switch proof.Origin {
	case "native":
		if tables.Archive || tables.Seal || proof.SourceVersion != nil || proof.SourceRowCount != nil || proof.SourceSHA256 != nil {
			return 0, ErrLegacyAbilityRetirement
		}
		return legacyAbilityNative, nil
	case "legacy":
		if !tables.Archive || !tables.Seal || proof.SourceVersion == nil || proof.SourceRowCount == nil || proof.SourceSHA256 == nil {
			return 0, ErrLegacyAbilityRetirement
		}
		seal, err := verifyLegacyAbilitySourceArchiveTx(tx)
		if err != nil || *proof.SourceVersion != seal.Version || *proof.SourceRowCount != seal.RowCount || *proof.SourceSHA256 != seal.SHA256 {
			return 0, ErrLegacyAbilityRetirement
		}
		var completed int64
		if err := tx.Model(&legacyGatewayMigration{}).Where("version = ?", legacyGatewayMigrationVersion).Count(&completed).Error; err != nil || completed != 1 {
			return 0, ErrLegacyAbilityRetirement
		}
		return legacyAbilityRetired, nil
	default:
		return 0, ErrLegacyAbilityRetirement
	}
}

// Inspect the selected application schema, not unrelated tenants' schemas. Any
// application object or offline-import namespace is evidence against freshness.
// Extension-owned objects are not application data.
func emptyNativeApplicationSchema(tx *gorm.DB) (bool, error) {
	var empty bool
	err := tx.Raw(`SELECT NOT EXISTS (
 SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname=current_schema()
 AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname=current_schema()
 AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace
 WHERE n.nspname=current_schema() AND t.typrelid=0 AND t.typelem=0 AND t.typtype IN ('e','d','b','r','m')
 AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid='pg_type'::regclass AND d.objid=t.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM (
 SELECT oid,collnamespace AS namespace,'pg_collation'::regclass AS catalog FROM pg_collation
 UNION ALL SELECT oid,oprnamespace,'pg_operator'::regclass FROM pg_operator
 UNION ALL SELECT oid,opcnamespace,'pg_opclass'::regclass FROM pg_opclass
 UNION ALL SELECT oid,opfnamespace,'pg_opfamily'::regclass FROM pg_opfamily
 UNION ALL SELECT oid,connamespace,'pg_conversion'::regclass FROM pg_conversion
 UNION ALL SELECT oid,cfgnamespace,'pg_ts_config'::regclass FROM pg_ts_config
 UNION ALL SELECT oid,dictnamespace,'pg_ts_dict'::regclass FROM pg_ts_dict
 UNION ALL SELECT oid,prsnamespace,'pg_ts_parser'::regclass FROM pg_ts_parser
 UNION ALL SELECT oid,tmplnamespace,'pg_ts_template'::regclass FROM pg_ts_template
 UNION ALL SELECT oid,stxnamespace,'pg_statistic_ext'::regclass FROM pg_statistic_ext
 ) app JOIN pg_namespace n ON n.oid=app.namespace
 WHERE n.nspname=current_schema()
 AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid=app.catalog AND d.objid=app.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM pg_namespace WHERE nspname='gateway_migration' OR left(nspname,14)='legacy_import_'
) AS empty`).Scan(&empty).Error
	if err != nil {
		return false, ErrLegacyAbilityRetirement
	}
	return empty, nil
}

func writeLegacyAbilityRetirementTx(tx *gorm.DB, seal *LegacyAbilitySourceManifest) error {
	if err := tx.AutoMigrate(&legacyAbilityRetirement{}); err != nil {
		return ErrLegacyAbilityRetirement
	}
	if err := tx.Exec(`ALTER TABLE legacy_ability_retirements ADD CONSTRAINT legacy_ability_retirement_origin CHECK (
 (origin='native' AND source_version IS NULL AND source_row_count IS NULL AND source_sha256 IS NULL) OR
 (origin='legacy' AND source_version IS NOT NULL AND source_version=1 AND source_row_count IS NOT NULL AND source_row_count>=0
 AND source_sha256 IS NOT NULL AND source_sha256 ~ '^[0-9a-f]{64}$'));
 CREATE FUNCTION protect_legacy_ability_retirement() RETURNS trigger AS $$ BEGIN
 RAISE EXCEPTION 'legacy Ability retirement is immutable'; END; $$ LANGUAGE plpgsql;
 CREATE TRIGGER legacy_ability_retirement_immutable BEFORE UPDATE OR DELETE ON legacy_ability_retirements
 FOR EACH ROW EXECUTE FUNCTION protect_legacy_ability_retirement();
 CREATE TRIGGER legacy_ability_retirement_no_truncate BEFORE TRUNCATE ON legacy_ability_retirements
 FOR EACH STATEMENT EXECUTE FUNCTION protect_legacy_ability_retirement();`).Error; err != nil {
		return ErrLegacyAbilityRetirement
	}
	proof := legacyAbilityRetirement{ID: 1, Version: 1, Origin: "native", CompletedAt: clock.Now().Unix()}
	if seal != nil {
		proof.Origin = "legacy"
		proof.SourceVersion, proof.SourceRowCount, proof.SourceSHA256 = &seal.Version, &seal.RowCount, &seal.SHA256
	}
	if err := tx.Create(&proof).Error; err != nil {
		return ErrLegacyAbilityRetirement
	}
	return nil
}

// Fresh primary initialization is atomic, including Key policies and proof. A
// failed attempt leaves no partial schema to misclassify on its next startup.
func initializeNativeDatabase(db *gorm.DB) error {
	return db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510050)`).Error; err != nil {
			return ErrLegacyAbilityRetirement
		}
		state, err := inspectLegacyAbilityRetirementTx(tx)
		if err != nil {
			return err
		}
		if state != legacyAbilityEmpty && state != legacyAbilityNative {
			return ErrLegacyAbilityRetirement
		}
		previous := DB
		DB = tx
		defer func() { DB = previous }()
		if err := migrateDB(); err != nil {
			return err
		}
		if err := MigrateKeyPermissions(tx); err != nil {
			return err
		}
		if state == legacyAbilityNative {
			return nil
		}
		return writeLegacyAbilityRetirementTx(tx, nil)
	})
}

func retireLegacyAbilitySourceTx(tx *gorm.DB, original *LegacyAbilitySourceManifest) error {
	seal, err := ArchiveLegacyAbilitySource(tx)
	if err != nil {
		return err
	}
	if original == nil || original.Version != seal.Version || original.RowCount != seal.RowCount || original.SHA256 != seal.SHA256 {
		return ErrLegacyAbilityRetirement
	}
	// No CASCADE: unknown dependants block retirement, preserving the whole source.
	if err := tx.Exec(`DROP TABLE "abilities"`).Error; err != nil {
		return ErrLegacyAbilityRetirement
	}
	return writeLegacyAbilityRetirementTx(tx, seal)
}
