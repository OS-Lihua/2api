package model

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var ErrLegacyAbilitySource = errors.New("legacy ability source cannot be verified")

// LegacyAbilitySource preserves source values, not routing authority. In
// particular, NULL, false, zero, negative integers and orphan identities remain
// distinct. The ordinal preserves multiplicity without inventing source keys.
// No Provider data or foreign keys belong in this archive.
type LegacyAbilitySource struct {
	Ordinal   int64   `gorm:"primaryKey;autoIncrement:false;check:legacy_ability_source_ordinal,ordinal > 0"`
	Group     *string `gorm:"column:group;type:text"`
	Model     *string `gorm:"type:text"`
	ChannelID *int64  `gorm:"type:bigint"`
	Enabled   *bool
	Priority  *int64  `gorm:"type:bigint"`
	Weight    *int64  `gorm:"type:bigint"`
	Tag       *string `gorm:"type:text"`
}

// LegacyAbilitySourceManifest seals one complete capture. It is not the gateway
// migration marker, permission to drop the source, or proof of a fresh database.
type LegacyAbilitySourceManifest struct {
	ID         int    `gorm:"primaryKey;autoIncrement:false;check:legacy_ability_source_manifest_id,id = 1"`
	Version    int    `gorm:"not null;check:legacy_ability_source_version,version = 1"`
	RowCount   int64  `gorm:"not null;check:legacy_ability_source_count,row_count >= 0"`
	SHA256     string `gorm:"type:char(64);not null;check:legacy_ability_source_digest,sha256 ~ '^[0-9a-f]{64}$'"`
	CapturedAt int64  `gorm:"not null"`
}

// ArchiveLegacyAbilitySource captures or fully verifies a sealed legacy source.
// It requires an existing abilities table, even on replay. There is deliberately
// no missing-table/fresh/after-drop fallback. Passing an outer transaction keeps
// the source lock and all archive changes inside that transaction; this method
// uses a savepoint and never commits the caller's transaction.
//
// Only this function installs the archive schema. It has no startup registration
// and never changes abilities, runtime caches, Provider records or money.
func ArchiveLegacyAbilitySource(db *gorm.DB) (*LegacyAbilitySourceManifest, error) {
	if db == nil || db.Config == nil || db.Dialector == nil || db.Dialector.Name() != "postgres" {
		return nil, fmt.Errorf("%w: PostgreSQL is required", ErrLegacyAbilitySource)
	}
	var result LegacyAbilitySourceManifest
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) (captureErr error) {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510049)`).Error; err != nil {
			return fmt.Errorf("%w: cannot lock capture", ErrLegacyAbilitySource)
		}
		// Query errors must not become HasTable=false. Lock before inspecting
		// columns or reading rows, preventing source DDL and writes until commit.
		var present bool
		if err := tx.Raw(`SELECT to_regclass('abilities') IS NOT NULL`).Scan(&present).Error; err != nil {
			return fmt.Errorf("%w: cannot inspect source", ErrLegacyAbilitySource)
		}
		if !present {
			return fmt.Errorf("%w: abilities is missing", ErrLegacyAbilitySource)
		}
		if err := tx.Exec(`LOCK TABLE "abilities" IN SHARE ROW EXCLUSIVE MODE`).Error; err != nil {
			return fmt.Errorf("%w: cannot lock source", ErrLegacyAbilitySource)
		}
		if err := requireUnfilteredLegacyAbilityTables(tx, "abilities"); err != nil {
			return err
		}
		var columns []struct {
			Name string
			Kind string
		}
		if err := tx.Raw(`SELECT a.attname AS name, t.typname AS kind
FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
JOIN pg_class c ON c.oid = a.attrelid
WHERE a.attrelid = to_regclass('abilities') AND c.relkind = 'r'
 AND a.attnum > 0 AND NOT a.attisdropped`).Scan(&columns).Error; err != nil {
			return fmt.Errorf("%w: cannot read source schema", ErrLegacyAbilitySource)
		}
		required := map[string]bool{"group": false, "model": false, "channel_id": false, "enabled": false, "priority": false, "weight": false, "tag": false}
		for _, column := range columns {
			if _, needed := required[column.Name]; !needed {
				return fmt.Errorf("%w: unsupported source column", ErrLegacyAbilitySource)
			}
			valid := false
			switch column.Name {
			case "group", "model", "tag":
				valid = column.Kind == "text" || column.Kind == "varchar"
			case "enabled":
				valid = column.Kind == "bool"
			default:
				valid = column.Kind == "int2" || column.Kind == "int4" || column.Kind == "int8"
			}
			if !valid {
				return fmt.Errorf("%w: unsupported source column type", ErrLegacyAbilitySource)
			}
			required[column.Name] = true
		}
		for _, found := range required {
			if !found {
				return fmt.Errorf("%w: source column is missing", ErrLegacyAbilitySource)
			}
		}

		var tables struct{ Rows, Manifest bool }
		if err := tx.Raw(`SELECT to_regclass('legacy_ability_sources') IS NOT NULL AS rows,
 to_regclass('legacy_ability_source_manifests') IS NOT NULL AS manifest`).Scan(&tables).Error; err != nil {
			return fmt.Errorf("%w: cannot inspect archive", ErrLegacyAbilitySource)
		}
		if tables.Rows != tables.Manifest {
			return fmt.Errorf("%w: incomplete archive schema", ErrLegacyAbilitySource)
		}
		if !tables.Rows {
			if err := tx.AutoMigrate(&LegacyAbilitySource{}, &LegacyAbilitySourceManifest{}); err != nil {
				return fmt.Errorf("%w: cannot create archive", ErrLegacyAbilitySource)
			}
			if err := tx.Exec(`CREATE FUNCTION protect_legacy_ability_source() RETURNS trigger AS $$
BEGIN
 IF TG_OP = 'INSERT' THEN
  IF EXISTS (SELECT 1 FROM "legacy_ability_source_manifests") THEN
   RAISE EXCEPTION 'legacy ability source is sealed';
  END IF;
  RETURN NEW;
 END IF;
 RAISE EXCEPTION 'legacy ability source is immutable';
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER legacy_ability_source_immutable BEFORE INSERT OR UPDATE OR DELETE ON "legacy_ability_sources"
 FOR EACH ROW EXECUTE FUNCTION protect_legacy_ability_source();
CREATE TRIGGER legacy_ability_source_no_truncate BEFORE TRUNCATE ON "legacy_ability_sources"
 FOR EACH STATEMENT EXECUTE FUNCTION protect_legacy_ability_source();
CREATE TRIGGER legacy_ability_manifest_immutable BEFORE UPDATE OR DELETE ON "legacy_ability_source_manifests"
 FOR EACH ROW EXECUTE FUNCTION protect_legacy_ability_source();
CREATE TRIGGER legacy_ability_manifest_no_truncate BEFORE TRUNCATE ON "legacy_ability_source_manifests"
 FOR EACH STATEMENT EXECUTE FUNCTION protect_legacy_ability_source();`).Error; err != nil {
				return fmt.Errorf("%w: cannot protect archive", ErrLegacyAbilitySource)
			}
		}
		if err := tx.Exec(`LOCK TABLE "legacy_ability_sources", "legacy_ability_source_manifests" IN SHARE ROW EXCLUSIVE MODE`).Error; err != nil {
			return fmt.Errorf("%w: cannot lock archive", ErrLegacyAbilitySource)
		}
		if err := requireUnfilteredLegacyAbilityTables(tx, "legacy_ability_sources", "legacy_ability_source_manifests"); err != nil {
			return err
		}
		if tables.Rows {
			verified, err := verifyLegacyAbilitySourceArchiveTx(tx)
			if err != nil {
				return err
			}
			result = *verified
		}
		// A server cursor bounds each client batch without OFFSET rescans. C
		// collation and explicit NULL ordering make the digest independent of
		// locale and physical row order, including identical duplicate rows.
		if err := tx.Exec(`DECLARE legacy_ability_source_cursor NO SCROLL CURSOR FOR
SELECT "group", "model", "channel_id", "enabled", "priority", "weight", "tag" FROM "abilities"
ORDER BY "group" COLLATE "C" NULLS FIRST, "model" COLLATE "C" NULLS FIRST,
 "channel_id" NULLS FIRST, "enabled" NULLS FIRST, "priority" NULLS FIRST,
 "weight" NULLS FIRST, "tag" COLLATE "C" NULLS FIRST`).Error; err != nil {
			return fmt.Errorf("%w: cannot read source", ErrLegacyAbilitySource)
		}
		defer func() {
			if err := tx.Exec(`CLOSE legacy_ability_source_cursor`).Error; err != nil && captureErr == nil {
				captureErr = fmt.Errorf("%w: cannot close source cursor", ErrLegacyAbilitySource)
			}
		}()
		digest := sha256.New()
		_, _ = digest.Write([]byte("legacy-ability-source:v1\n"))
		var count int64
		for {
			var batch []LegacyAbilitySource
			if err := tx.Raw(`FETCH FORWARD 250 FROM legacy_ability_source_cursor`).Scan(&batch).Error; err != nil {
				return fmt.Errorf("%w: cannot read source batch", ErrLegacyAbilitySource)
			}
			if len(batch) == 0 {
				break
			}
			first := count + 1
			for i := range batch {
				count++
				batch[i].Ordinal = count
			}
			if !tables.Rows {
				if err := tx.CreateInBatches(&batch, 250).Error; err != nil {
					return fmt.Errorf("%w: cannot capture source batch", ErrLegacyAbilitySource)
				}
			}
			var archived []LegacyAbilitySource
			if err := tx.Where("ordinal BETWEEN ? AND ?", first, count).Order("ordinal").Limit(250).Find(&archived).Error; err != nil || len(archived) != len(batch) {
				return fmt.Errorf("%w: archive rows differ", ErrLegacyAbilitySource)
			}
			for i := range batch {
				sourceJSON, err := common.Marshal(batch[i])
				if err != nil {
					return fmt.Errorf("%w: cannot encode source evidence", ErrLegacyAbilitySource)
				}
				archiveJSON, err := common.Marshal(archived[i])
				if err != nil || !bytes.Equal(sourceJSON, archiveJSON) {
					return fmt.Errorf("%w: archive row differs", ErrLegacyAbilitySource)
				}
				_, _ = digest.Write(sourceJSON)
				_, _ = digest.Write([]byte{'\n'})
			}
		}
		var archivedCount int64
		if err := tx.Model(&LegacyAbilitySource{}).Count(&archivedCount).Error; err != nil || archivedCount != count {
			return fmt.Errorf("%w: archive row count differs", ErrLegacyAbilitySource)
		}
		sum := hex.EncodeToString(digest.Sum(nil))
		if tables.Rows {
			if result.RowCount != count || result.SHA256 != sum {
				return fmt.Errorf("%w: archive seal differs", ErrLegacyAbilitySource)
			}
			return nil
		}
		result = LegacyAbilitySourceManifest{ID: 1, Version: 1, RowCount: count, SHA256: sum, CapturedAt: clock.Now().Unix()}
		if err := tx.Create(&result).Error; err != nil {
			return fmt.Errorf("%w: cannot seal archive", ErrLegacyAbilitySource)
		}
		return nil
	})
	if err != nil {
		// Driver and callback errors can contain arbitrary source values.
		if errors.Is(err, ErrLegacyAbilitySource) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: transaction failed", ErrLegacyAbilitySource)
	}
	return &result, nil
}

// VerifyLegacyAbilitySourceArchive verifies the sealed archive independently of
// the original table. It performs no DDL or writes and does not establish that
// source retirement or any other migration has completed. A missing archive or
// manifest is an error, never an empty-source result.
func VerifyLegacyAbilitySourceArchive(db *gorm.DB) (*LegacyAbilitySourceManifest, error) {
	if db == nil || db.Config == nil || db.Dialector == nil || db.Dialector.Name() != "postgres" {
		return nil, fmt.Errorf("%w: PostgreSQL is required", ErrLegacyAbilitySource)
	}
	var result *LegacyAbilitySourceManifest
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Transaction(func(tx *gorm.DB) error {
		var err error
		result, err = verifyLegacyAbilitySourceArchiveTx(tx)
		return err
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		if errors.Is(err, ErrLegacyAbilitySource) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: verification transaction failed", ErrLegacyAbilitySource)
	}
	return result, nil
}

// Call only after locking these relations against DDL. A filtered row set cannot
// prove completeness, even when its count and digest match a filtered seal. Do
// not depend on the caller being an owner, superuser or BYPASSRLS role, and never
// change security settings to make an unsupported source appear readable.
func requireUnfilteredLegacyAbilityTables(tx *gorm.DB, tables ...string) error {
	for _, table := range tables {
		var unfiltered bool
		if err := tx.Raw(`SELECT EXISTS (
	 SELECT 1 FROM pg_class c WHERE c.oid=to_regclass(?) AND c.relkind='r'
	 AND NOT c.relrowsecurity AND NOT c.relforcerowsecurity
	 AND NOT EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid))`, table).Scan(&unfiltered).Error; err != nil || !unfiltered {
			return fmt.Errorf("%w: unsupported table visibility", ErrLegacyAbilitySource)
		}
	}
	return nil
}

func verifyLegacyAbilitySourceArchiveTx(tx *gorm.DB) (*LegacyAbilitySourceManifest, error) {
	// ACCESS SHARE also prevents concurrent archive DDL while remaining legal
	// in a read-only transaction. Sealed rows and their manifest reject writes.
	if err := tx.Exec(`LOCK TABLE "legacy_ability_sources", "legacy_ability_source_manifests" IN ACCESS SHARE MODE`).Error; err != nil {
		return nil, fmt.Errorf("%w: archive is missing or unreadable", ErrLegacyAbilitySource)
	}
	if err := requireUnfilteredLegacyAbilityTables(tx, "legacy_ability_sources", "legacy_ability_source_manifests"); err != nil {
		return nil, err
	}
	var protected int64
	if err := tx.Raw(`SELECT COUNT(*) FROM pg_trigger
WHERE NOT tgisinternal AND tgenabled IN ('O','A') AND tgfoid = to_regprocedure('protect_legacy_ability_source()')
 AND ((tgrelid = to_regclass('legacy_ability_sources') AND tgname IN ('legacy_ability_source_immutable','legacy_ability_source_no_truncate'))
 OR (tgrelid = to_regclass('legacy_ability_source_manifests') AND tgname IN ('legacy_ability_manifest_immutable','legacy_ability_manifest_no_truncate')))`).Scan(&protected).Error; err != nil || protected != 4 {
		return nil, fmt.Errorf("%w: archive protection differs", ErrLegacyAbilitySource)
	}
	var manifests []LegacyAbilitySourceManifest
	if err := tx.Limit(2).Find(&manifests).Error; err != nil || len(manifests) != 1 {
		return nil, fmt.Errorf("%w: archive seal is missing or unreadable", ErrLegacyAbilitySource)
	}
	result := manifests[0]
	if result.ID != 1 || result.Version != 1 || result.RowCount < 0 {
		return nil, fmt.Errorf("%w: unsupported archive seal", ErrLegacyAbilitySource)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("legacy-ability-source:v1\n"))
	var count int64
	for {
		var batch []LegacyAbilitySource
		if err := tx.Where("ordinal > ?", count).Order("ordinal").Limit(250).Find(&batch).Error; err != nil {
			return nil, fmt.Errorf("%w: cannot read archive batch", ErrLegacyAbilitySource)
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			count++
			if row.Ordinal != count {
				return nil, fmt.Errorf("%w: archive sequence differs", ErrLegacyAbilitySource)
			}
			encoded, err := common.Marshal(row)
			if err != nil {
				return nil, fmt.Errorf("%w: cannot encode archive evidence", ErrLegacyAbilitySource)
			}
			_, _ = digest.Write(encoded)
			_, _ = digest.Write([]byte{'\n'})
		}
	}
	var actualCount int64
	if err := tx.Model(&LegacyAbilitySource{}).Count(&actualCount).Error; err != nil || actualCount != count || result.RowCount != count || result.SHA256 != hex.EncodeToString(digest.Sum(nil)) {
		return nil, fmt.Errorf("%w: archive seal differs", ErrLegacyAbilitySource)
	}
	return &result, nil
}
