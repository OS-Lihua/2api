package model

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func legacyAbilityArchiveDB(t *testing.T, count int) *gorm.DB {
	t.Helper()
	db := UseTestDatabases(t)
	require.NoError(t, db.AutoMigrate(&Ability{}))
	if count > 0 {
		require.NoError(t, db.Exec(`INSERT INTO "abilities" ("group","model","channel_id","enabled","priority","weight","tag")
SELECT 'group-' || (i % 3), 'model-' || lpad(i::text,4,'0'), 900000 + i,
 CASE WHEN i % 3 = 0 THEN NULL ELSE i % 2 = 0 END,
 CASE WHEN i % 4 = 0 THEN NULL ELSE -i END,
 CASE WHEN i % 5 = 0 THEN NULL ELSE i - 125 END,
 CASE WHEN i % 6 = 0 THEN NULL ELSE 'tag-' || i END
FROM generate_series(1, ?) AS i`, count).Error)
	}
	return db
}

func legacyAbilitySourceState(t *testing.T, db *gorm.DB) string {
	t.Helper()
	var state string
	require.NoError(t, db.Raw(`SELECT COALESCE(json_agg(row_to_json(s) ORDER BY
 s."group" COLLATE "C" NULLS FIRST, s."model" COLLATE "C" NULLS FIRST,
 s."channel_id" NULLS FIRST, s."enabled" NULLS FIRST, s."priority" NULLS FIRST,
 s."weight" NULLS FIRST, s."tag" COLLATE "C" NULLS FIRST)::text, '[]')
FROM (SELECT "group","model","channel_id","enabled","priority","weight","tag" FROM "abilities") s`).Scan(&state).Error)
	return state
}

func assertLegacyAbilityArchiveEqual(t *testing.T, db *gorm.DB) {
	t.Helper()
	var differences int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM (
 (SELECT "group","model","channel_id","enabled","priority","weight","tag" FROM "abilities"
 EXCEPT ALL SELECT "group","model","channel_id","enabled","priority","weight","tag" FROM "legacy_ability_sources")
 UNION ALL
 (SELECT "group","model","channel_id","enabled","priority","weight","tag" FROM "legacy_ability_sources"
 EXCEPT ALL SELECT "group","model","channel_id","enabled","priority","weight","tag" FROM "abilities")
) differences`).Scan(&differences).Error)
	assert.Zero(t, differences, "every source value and duplicate must survive")
}

// The caller remains the owner of the fixture transaction. Only the operation
// under test runs as this non-superuser owner, with FORCE RLS actually filtering.
func legacyAbilityFilteredOwner(t *testing.T, tx *gorm.DB, table, predicate string) string {
	t.Helper()
	require.Contains(t, []string{"abilities", "legacy_ability_sources", "legacy_ability_source_manifests"}, table)
	var role, owner string
	require.NoError(t, tx.Raw(`SELECT 'abilityfilter_' || substr(md5(current_schema() || ?),1,16)`, table).Scan(&role).Error)
	require.NoError(t, tx.Raw(`SELECT current_user`).Scan(&owner).Error)
	require.NoError(t, tx.Exec(fmt.Sprintf(`CREATE ROLE %q NOLOGIN NOSUPERUSER NOBYPASSRLS; GRANT %q TO %q; ALTER TABLE %q OWNER TO %q`, role, owner, role, table, role)).Error)
	require.NoError(t, tx.Exec(fmt.Sprintf(`ALTER TABLE %q ENABLE ROW LEVEL SECURITY; ALTER TABLE %q FORCE ROW LEVEL SECURITY; CREATE POLICY filtered_evidence ON %q USING (%s)`, table, table, table, predicate)).Error)
	return role
}

func legacyAbilityCompleteTableState(t *testing.T, tx *gorm.DB, table string) string {
	t.Helper()
	require.Contains(t, []string{"abilities", "legacy_ability_sources", "legacy_ability_source_manifests"}, table)
	var state string
	require.NoError(t, tx.Raw(fmt.Sprintf(`SELECT jsonb_build_object(
	 'rows',(SELECT COALESCE(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text),'[]'::jsonb) FROM %q r),
	 'columns',(SELECT jsonb_agg(jsonb_build_object('name',a.attname,'type',a.atttypid,'not_null',a.attnotnull) ORDER BY a.attnum) FROM pg_attribute a WHERE a.attrelid=to_regclass(?) AND a.attnum>0 AND NOT a.attisdropped),
	 'security',(SELECT jsonb_build_object('enabled',c.relrowsecurity,'forced',c.relforcerowsecurity,'owner',c.relowner) FROM pg_class c WHERE c.oid=to_regclass(?)),
	 'policies',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY p.oid),'[]'::jsonb) FROM pg_policy p WHERE p.polrelid=to_regclass(?)))::text`, table), table, table, table).Scan(&state).Error)
	return state
}

func TestLegacyAbilityRetirementRejectsIncompleteSource(t *testing.T) {
	for _, fault := range []string{"unknown_source_column", "forced_row_security"} {
		t.Run(fault, func(t *testing.T) {
			db := legacyAbilityRetirementFixture(t)
			rollback := errors.New("fixture rollback")
			err := db.Transaction(func(tx *gorm.DB) error {
				previous := DB
				DB = tx
				defer func() { DB = previous }()
				role := ""
				if fault == "unknown_source_column" {
					require.NoError(t, tx.Exec(`ALTER TABLE abilities ADD COLUMN archival_annotation text NOT NULL DEFAULT 'source-column-must-survive'`).Error)
				} else {
					role = legacyAbilityFilteredOwner(t, tx, "abilities", "false")
				}
				before := legacyAbilityCompleteTableState(t, tx, "abilities")
				var physicalCount int64
				require.NoError(t, tx.Table("abilities").Count(&physicalCount).Error)
				require.EqualValues(t, 1, physicalCount)
				if role != "" {
					require.NoError(t, tx.Exec(fmt.Sprintf(`SET LOCAL ROLE %q`, role)).Error)
					var visibleCount int64
					require.NoError(t, tx.Table("abilities").Count(&visibleCount).Error)
					require.Zero(t, visibleCount, "the legal owner really cannot see the stored row")
				}
				// Check the standalone boundary too, before any archive DDL persists.
				require.NoError(t, tx.Exec(`SAVEPOINT completeness_probe`).Error)
				seal, captureErr := ArchiveLegacyAbilitySource(tx)
				assert.ErrorIs(t, captureErr, ErrLegacyAbilitySource)
				assert.Nil(t, seal)
				assert.False(t, tx.Migrator().HasTable("legacy_ability_sources"))
				assert.False(t, tx.Migrator().HasTable("legacy_ability_source_manifests"))
				require.NoError(t, tx.Exec(`ROLLBACK TO SAVEPOINT completeness_probe; RELEASE SAVEPOINT completeness_probe`).Error)

				report, migrationErr := MigrateLegacyGateway(t.Context())
				assert.Error(t, migrationErr)
				assert.False(t, report.Committed, "an outer fixture rollback does not excuse a successful destructive migration")
				if role != "" {
					require.NoError(t, tx.Exec(`RESET ROLE`).Error)
				}
				if assert.True(t, tx.Migrator().HasTable("abilities")) {
					assert.Equal(t, before, legacyAbilityCompleteTableState(t, tx, "abilities"), "preserve all columns, actual rows, ownership and RLS policies")
				}
				for _, table := range []string{"legacy_ability_sources", "legacy_ability_source_manifests", "legacy_ability_retirements", "legacy_gateway_migrations"} {
					assert.False(t, tx.Migrator().HasTable(table), table)
				}
				return rollback
			})
			require.ErrorIs(t, err, rollback)
		})
	}
}

func TestLegacyAbilitySourceRejectsFilteredEvidence(t *testing.T) {
	for _, table := range []string{"legacy_ability_sources", "legacy_ability_source_manifests"} {
		t.Run(table, func(t *testing.T) {
			db := legacyAbilityArchiveDB(t, 1)
			_, err := ArchiveLegacyAbilitySource(db)
			require.NoError(t, err)
			rollback := errors.New("fixture rollback")
			err = db.Transaction(func(tx *gorm.DB) error {
				predicate := "true"
				if table == "legacy_ability_sources" {
					// A hidden extra row makes both the filtered count and digest look
					// like the original one-row seal. Re-enable protection before use.
					require.NoError(t, tx.Exec(`ALTER TABLE legacy_ability_sources DISABLE TRIGGER legacy_ability_source_immutable;
					 INSERT INTO legacy_ability_sources SELECT 2,"group",model,channel_id,enabled,priority,weight,tag FROM legacy_ability_sources WHERE ordinal=1;
					 ALTER TABLE legacy_ability_sources ENABLE TRIGGER legacy_ability_source_immutable`).Error)
					predicate = "ordinal=1"
				}
				role := legacyAbilityFilteredOwner(t, tx, table, predicate)
				before := make(map[string]string)
				for _, name := range []string{"abilities", "legacy_ability_sources", "legacy_ability_source_manifests"} {
					before[name] = legacyAbilityCompleteTableState(t, tx, name)
				}
				require.NoError(t, tx.Exec(fmt.Sprintf(`SET LOCAL ROLE %q`, role)).Error)
				var visibleCount int64
				require.NoError(t, tx.Table(table).Count(&visibleCount).Error)
				require.EqualValues(t, 1, visibleCount)
				for range 2 {
					verified, verifyErr := VerifyLegacyAbilitySourceArchive(tx)
					assert.ErrorIs(t, verifyErr, ErrLegacyAbilitySource)
					assert.Nil(t, verified)
					replayed, captureErr := ArchiveLegacyAbilitySource(tx)
					assert.ErrorIs(t, captureErr, ErrLegacyAbilitySource)
					assert.Nil(t, replayed)
				}
				require.NoError(t, tx.Exec(`RESET ROLE`).Error)
				for name, state := range before {
					assert.Equal(t, state, legacyAbilityCompleteTableState(t, tx, name), "filtered evidence must not be repaired or rewritten")
				}
				return rollback
			})
			require.ErrorIs(t, err, rollback)
		})
	}
}

func TestLegacyAbilityRetirementFreshIgnoresExtensionObjects(t *testing.T) {
	db := UseTestDatabases(t)
	require.NoError(t, db.Exec(`DO $$ BEGIN EXECUTE format('CREATE EXTENSION hstore WITH SCHEMA %I', current_schema()); END $$`).Error)
	t.Cleanup(func() { require.NoError(t, db.Exec(`DROP EXTENSION hstore`).Error) })
	empty, err := emptyNativeApplicationSchema(db)
	require.NoError(t, err)
	assert.True(t, empty, "system and extension-owned types, operators and functions are not application evidence")
	require.NoError(t, db.Exec(`CREATE TYPE application_record AS (id bigint)`).Error)
	empty, err = emptyNativeApplicationSchema(db)
	require.NoError(t, err)
	assert.False(t, empty, "an application-owned composite type is not a fresh database")
}

func TestLegacyAbilityRetirementFreshAtomicity(t *testing.T) {
	for _, phase := range []string{"key_permissions", "proof"} {
		t.Run(phase, func(t *testing.T) {
			db := UseTestDatabases(t)
			t.Setenv("SETTLEMENT_CURRENCY", "USD")
			require.NoError(t, InitSettlementCurrency())
			callback := "test:fresh_ability_failure"
			triggered := false
			fail := func(tx *gorm.DB) {
				isProofWrite := phase == "proof" && tx.Statement.Table == "legacy_ability_retirements"
				isKeySchemaRead := phase == "key_permissions" && strings.Contains(tx.Statement.SQL.String(), "'model_limits_enabled'")
				if isProofWrite || isKeySchemaRead {
					triggered = true
					tx.AddError(tx.Exec(`SELECT 1/0`).Error)
				}
			}
			if phase == "proof" {
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, fail))
			} else {
				require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callback, fail))
			}
			previous := DB
			require.Error(t, initializeNativeDatabase(db))
			assert.True(t, triggered, "inject an actual failure after primary schema creation")
			assert.Same(t, previous, DB, "restore binding even on failed initialization")
			if phase == "proof" {
				require.NoError(t, db.Callback().Create().Remove(callback))
			} else {
				require.NoError(t, db.Callback().Row().Remove(callback))
			}
			empty, err := emptyNativeApplicationSchema(db)
			require.NoError(t, err)
			assert.True(t, empty, "schema, routines, all money rows and proof must roll back together")
			require.NoError(t, initializeNativeDatabase(db))
			assert.Same(t, previous, DB)
			state, err := inspectLegacyAbilityStartup(db)
			require.NoError(t, err)
			assert.Equal(t, legacyAbilityNative, state)
			assert.False(t, db.Migrator().HasTable("abilities"))
			assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
			assert.False(t, db.Migrator().HasTable(&legacyGatewayMigration{}))
			var proof legacyAbilityRetirement
			require.NoError(t, db.First(&proof).Error)
			assert.Equal(t, "native", proof.Origin)
			assert.Nil(t, proof.SourceVersion)
			assert.Nil(t, proof.SourceRowCount)
			assert.Nil(t, proof.SourceSHA256)
			for range 2 {
				report, err := MigrateLegacyGateway(t.Context())
				require.NoError(t, err)
				assert.True(t, report.Committed)
				assert.True(t, report.AlreadyCompleted)
				assert.Empty(t, report.Counts)
				assert.False(t, db.Migrator().HasTable(&legacyGatewayMigration{}))
			}
		})
	}
}

func legacyAbilityRetirementFixture(t *testing.T) *gorm.DB {
	t.Helper()
	db := UseTestDatabases(t)
	t.Setenv("SETTLEMENT_CURRENCY", "USD")
	require.NoError(t, InitSettlementCurrency())
	require.NoError(t, initializeNativeDatabase(db))
	// Simulate an installation predating this retirement. Never used in production.
	require.NoError(t, db.Exec(`DROP TABLE legacy_ability_retirements; DROP FUNCTION protect_legacy_ability_retirement()`).Error)
	require.NoError(t, db.AutoMigrate(&Ability{}))
	require.NoError(t, db.Exec(`ALTER TABLE "providers" ADD COLUMN "models" text, ADD COLUMN "group" text,
		ADD COLUMN "model_mapping" text, ADD COLUMN "priority" bigint, ADD COLUMN "weight" bigint`).Error)
	require.NoError(t, db.Create(&User{Id: 41, Username: "ability-root", Password: "fixture-hash", Role: 100, Status: 1, AffCode: "ability-root", Quota: 500000}).Error)
	require.NoError(t, db.Exec(`INSERT INTO providers(id,type,"key",status,name,models,"group",model_mapping,setting,param_override)
 VALUES (51,1,'',1,'archive-provider','archive-model','default','{}','{}','{}');
 INSERT INTO abilities("group",model,channel_id,enabled,priority,weight,tag) VALUES ('default','archive-model',51,false,NULL,-1,NULL);
 INSERT INTO options(key,value) VALUES ('ModelRatio','{"archive-model":0.125}'),('CompletionRatio','{"archive-model":2}')`).Error)
	return db
}

func TestLegacyAbilityRetirementOfflineRollback(t *testing.T) {
	for _, phase := range []string{"drop", "proof"} {
		t.Run(phase, func(t *testing.T) {
			db := legacyAbilityRetirementFixture(t)
			before := legacyAbilitySourceState(t, db)
			callback := "test:ability_retirement_failure"
			triggered := false
			if phase == "drop" {
				// A real dependency must block DROP; CASCADE would destroy this fact.
				require.NoError(t, db.Exec(`CREATE VIEW source_dependency AS SELECT * FROM abilities`).Error)
			} else {
				require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
					if tx.Statement.Table == "legacy_ability_retirements" {
						triggered = true
						tx.AddError(tx.Exec(`SELECT 1/0`).Error)
					}
				}))
			}
			report, err := MigrateLegacyGateway(t.Context())
			require.Error(t, err)
			assert.False(t, report.Committed)
			if phase == "proof" {
				assert.True(t, triggered, "reach the post-DROP proof insert")
				require.NoError(t, db.Callback().Create().Remove(callback))
			}
			assert.Equal(t, before, legacyAbilitySourceState(t, db))
			for _, table := range []string{"legacy_ability_retirements", "legacy_ability_sources", "legacy_ability_source_manifests", "legacy_gateway_migrations"} {
				assert.False(t, db.Migrator().HasTable(table), table)
			}
			for _, table := range []string{"model_offerings", "channels", "price_versions", "wallets", "ledger_entries"} {
				var count int64
				require.NoError(t, db.Table(table).Count(&count).Error)
				assert.Zero(t, count, table)
			}
			if phase == "drop" {
				require.NoError(t, db.Exec(`DROP VIEW source_dependency`).Error)
			}
			report, err = MigrateLegacyGateway(t.Context())
			require.NoError(t, err)
			assert.True(t, report.Committed)
			assert.EqualValues(t, 1, report.Counts["sale_prices_created"])
			assert.EqualValues(t, 1, report.Counts["legacy_ability_retired"])
			assert.False(t, db.Migrator().HasTable("abilities"))
			seal, err := VerifyLegacyAbilitySourceArchive(db)
			require.NoError(t, err)
			assert.EqualValues(t, 1, seal.RowCount)
			state, err := inspectLegacyAbilityStartup(db)
			require.NoError(t, err)
			assert.Equal(t, legacyAbilityRetired, state)
		})
	}
}

func TestLegacyAbilityRetirementCompletedAndCorruptProofs(t *testing.T) {
	db := legacyAbilityRetirementFixture(t)
	_, err := MigrateLegacyGateway(t.Context())
	require.NoError(t, err)
	// Model the already-completed v1 state produced by the previous executable.
	require.NoError(t, db.Exec(`CREATE TABLE abilities AS SELECT "group",model,channel_id,enabled,priority,weight,tag FROM legacy_ability_sources;
 DROP TABLE legacy_ability_retirements,legacy_ability_sources,legacy_ability_source_manifests;
 DROP FUNCTION protect_legacy_ability_retirement(); DROP FUNCTION protect_legacy_ability_source();
 ALTER TABLE "providers" ADD COLUMN "model_mapping" text;
 UPDATE "providers" SET "model_mapping"='{"archive-model":"cycle","cycle":"archive-model"}';
 UPDATE options SET value='{"archive-model":123}' WHERE key='ModelRatio'`).Error)
	snapshot := func() string {
		var state string
		require.NoError(t, db.Raw(`SELECT jsonb_build_object('offerings',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM model_offerings t),
 'channels',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM channels t),'prices',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM price_versions t),
 'wallets',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM wallets t),'ledger',(SELECT jsonb_agg(to_jsonb(t) ORDER BY id) FROM ledger_entries t))::text`).Scan(&state).Error)
		return state
	}
	before := snapshot()
	for iteration := range 2 {
		report, err := MigrateLegacyGateway(t.Context())
		require.NoError(t, err)
		assert.True(t, report.Committed)
		assert.Equal(t, iteration == 1, report.AlreadyCompleted)
		assert.Zero(t, report.Counts["sale_prices_created"])
		assert.Equal(t, before, snapshot(), "do not reopen pricing or credit an opening twice")
	}
	for _, statement := range []string{
		`UPDATE legacy_ability_retirements SET completed_at=completed_at+1`, `DELETE FROM legacy_ability_retirements`, `TRUNCATE legacy_ability_retirements`,
	} {
		assert.Error(t, db.Exec(statement).Error)
	}
	for _, fault := range []string{"source_reappears", "missing_seal", "archive_drift", "proof_binding", "proof_missing", "proof_protection"} {
		t.Run(fault, func(t *testing.T) {
			tx := db.Begin()
			require.NoError(t, tx.Error)
			defer tx.Rollback()
			switch fault {
			case "source_reappears":
				require.NoError(t, tx.AutoMigrate(&Ability{}))
			case "missing_seal":
				require.NoError(t, tx.Exec(`DROP TABLE legacy_ability_source_manifests`).Error)
			case "archive_drift":
				require.NoError(t, tx.Exec(`ALTER TABLE legacy_ability_sources DISABLE TRIGGER legacy_ability_source_immutable; UPDATE legacy_ability_sources SET weight=weight+1; ALTER TABLE legacy_ability_sources ENABLE TRIGGER legacy_ability_source_immutable`).Error)
			case "proof_binding":
				require.NoError(t, tx.Exec(`ALTER TABLE legacy_ability_retirements DISABLE TRIGGER legacy_ability_retirement_immutable; UPDATE legacy_ability_retirements SET source_sha256=repeat('0',64); ALTER TABLE legacy_ability_retirements ENABLE TRIGGER legacy_ability_retirement_immutable`).Error)
			case "proof_missing":
				require.NoError(t, tx.Exec(`DROP TABLE legacy_ability_retirements`).Error)
			case "proof_protection":
				require.NoError(t, tx.Exec(`ALTER TABLE legacy_ability_retirements DISABLE TRIGGER legacy_ability_retirement_immutable`).Error)
			}
			for range 2 {
				_, err := inspectLegacyAbilityStartup(tx)
				assert.ErrorIs(t, err, ErrLegacyAbilityRetirement)
				previous := DB
				DB = tx
				report, err := MigrateLegacyGateway(t.Context())
				DB = previous
				assert.Error(t, err)
				assert.False(t, report.Committed)
			}
		})
	}
	assert.Equal(t, before, snapshot())
}

func TestLegacyAbilitySourceCapturePreservesAllFacts(t *testing.T) {
	db := legacyAbilityArchiveDB(t, 250)
	var columns []struct {
		Name     string
		Kind     string
		Nullable bool
	}
	require.NoError(t, db.Raw(`SELECT a.attname AS name,t.typname AS kind,NOT a.attnotnull AS nullable
FROM pg_attribute a JOIN pg_type t ON t.oid=a.atttypid
WHERE a.attrelid=to_regclass('abilities') AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`).Scan(&columns).Error)
	require.Len(t, columns, 7)
	kinds := map[string]string{}
	nulls := map[string]bool{}
	for _, column := range columns {
		kinds[column.Name], nulls[column.Name] = column.Kind, column.Nullable
	}
	assert.Equal(t, map[string]string{"group": "varchar", "model": "varchar", "channel_id": "int8", "enabled": "bool", "priority": "int8", "weight": "int8", "tag": "text"}, kinds)
	assert.Equal(t, map[string]bool{"group": false, "model": false, "channel_id": false, "enabled": true, "priority": true, "weight": true, "tag": true}, nulls)
	// Preserve abnormal legacy identities too, rather than turning NULL keys
	// into empty strings/zero or using them as an archive primary key.
	require.NoError(t, db.Exec(`ALTER TABLE "abilities" DROP CONSTRAINT "abilities_pkey";
ALTER TABLE "abilities" ALTER COLUMN "group" DROP NOT NULL;
ALTER TABLE "abilities" ALTER COLUMN "model" DROP NOT NULL;
ALTER TABLE "abilities" ALTER COLUMN "channel_id" DROP NOT NULL;
INSERT INTO "abilities" VALUES (NULL,NULL,NULL,NULL,NULL,NULL,NULL),(NULL,NULL,NULL,NULL,NULL,NULL,NULL);
INSERT INTO "abilities" VALUES ('组😀','é/模型',-9223372036854775808,false,9223372036854775807,-9223372036854775808,E'\n\t  ');
INSERT INTO "abilities" VALUES ('组😀',U&'e\0301/模型',0,true,0,0,'');`).Error)
	before := legacyAbilitySourceState(t, db)
	// The implementation must use the caller's database, not global DB.
	previous := DB
	DB = nil
	t.Cleanup(func() { DB = previous })
	manifest, err := ArchiveLegacyAbilitySource(db)
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.EqualValues(t, 254, manifest.RowCount)
	assert.Regexp(t, `^[0-9a-f]{64}$`, manifest.SHA256)
	assert.Equal(t, before, legacyAbilitySourceState(t, db))
	assertLegacyAbilityArchiveEqual(t, db)
	assert.False(t, db.Migrator().HasTable("providers"), "orphan sources must not invent providers")

	var nullableCopies int64
	require.NoError(t, db.Model(&LegacyAbilitySource{}).Where(`"group" IS NULL AND "model" IS NULL AND "channel_id" IS NULL AND "enabled" IS NULL AND "priority" IS NULL AND "weight" IS NULL AND "tag" IS NULL`).Count(&nullableCopies).Error)
	assert.EqualValues(t, 2, nullableCopies)
	var extreme LegacyAbilitySource
	require.NoError(t, db.Where(`"channel_id" = -9223372036854775808`).Take(&extreme).Error)
	require.NotNil(t, extreme.Enabled)
	assert.False(t, *extreme.Enabled)
	require.NotNil(t, extreme.Weight)
	assert.EqualValues(t, -9223372036854775808, *extreme.Weight)

	replayed, err := ArchiveLegacyAbilitySource(db)
	require.NoError(t, err)
	assert.Equal(t, manifest, replayed)
	verified, err := VerifyLegacyAbilitySourceArchive(db)
	require.NoError(t, err)
	assert.Equal(t, manifest, verified)
	// Physical source order is not part of the evidence. Multiplicity is.
	require.NoError(t, db.Exec(`CREATE TEMP TABLE ability_reorder AS SELECT * FROM "abilities" ORDER BY "model" COLLATE "C" DESC NULLS LAST;
TRUNCATE "abilities";
INSERT INTO "abilities" SELECT * FROM ability_reorder;`).Error)
	reordered, err := ArchiveLegacyAbilitySource(db)
	require.NoError(t, err)
	assert.Equal(t, manifest, reordered)
	assert.Equal(t, before, legacyAbilitySourceState(t, db))
	assertLegacyAbilityArchiveEqual(t, db)

	// The independent verifier is suitable for a later retirement gate. This
	// test alone drops its isolated source; the production core never does.
	require.NoError(t, db.Exec(`DROP TABLE "abilities"`).Error)
	verified, err = VerifyLegacyAbilitySourceArchive(db)
	require.NoError(t, err)
	assert.Equal(t, manifest, verified)
	missing, err := ArchiveLegacyAbilitySource(db)
	assert.ErrorIs(t, err, ErrLegacyAbilitySource)
	assert.Nil(t, missing)
}

func TestLegacyAbilitySourceRejectsDriftAndProtectsEvidence(t *testing.T) {
	mutations := []struct{ name, sql string }{
		{"group", `UPDATE "abilities" SET "group"='changed' WHERE "channel_id"=900001`},
		{"model", `UPDATE "abilities" SET "model"='changed' WHERE "channel_id"=900001`},
		{"orphan_identity", `UPDATE "abilities" SET "channel_id"=-1 WHERE "channel_id"=900001`},
		{"enabled", `UPDATE "abilities" SET "enabled"=true WHERE "channel_id"=900001`},
		{"priority_null", `UPDATE "abilities" SET "priority"=NULL WHERE "channel_id"=900001`},
		{"weight_null", `UPDATE "abilities" SET "weight"=NULL WHERE "channel_id"=900001`},
		{"tag_null", `UPDATE "abilities" SET "tag"=NULL WHERE "channel_id"=900001`},
		{"append", `INSERT INTO "abilities" VALUES ('late','late',-9,false,NULL,NULL,NULL)`},
		{"delete", `DELETE FROM "abilities" WHERE "channel_id"=900001`},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			db := legacyAbilityArchiveDB(t, 2)
			manifest, err := ArchiveLegacyAbilitySource(db)
			require.NoError(t, err)
			require.NoError(t, db.Exec(tc.sql).Error)
			changed := legacyAbilitySourceState(t, db)
			for range 2 {
				result, err := ArchiveLegacyAbilitySource(db)
				assert.ErrorIs(t, err, ErrLegacyAbilitySource)
				assert.Nil(t, result)
				verified, err := VerifyLegacyAbilitySourceArchive(db)
				require.NoError(t, err)
				assert.Equal(t, manifest, verified)
				assert.Equal(t, changed, legacyAbilitySourceState(t, db), "verification cannot rewrite source")
			}
		})
	}
	t.Run("immutable_and_sealed", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 2)
		manifest, err := ArchiveLegacyAbilitySource(db)
		require.NoError(t, err)
		for _, query := range []string{
			`INSERT INTO "legacy_ability_sources" ("ordinal") VALUES (99)`,
			`UPDATE "legacy_ability_sources" SET "enabled"=true WHERE "ordinal"=1`,
			`DELETE FROM "legacy_ability_sources" WHERE "ordinal"=1`,
			`TRUNCATE "legacy_ability_sources"`,
			`UPDATE "legacy_ability_source_manifests" SET "row_count"=9`,
			`DELETE FROM "legacy_ability_source_manifests"`,
			`TRUNCATE "legacy_ability_source_manifests"`,
		} {
			assert.Error(t, db.Exec(query).Error)
		}
		verified, err := VerifyLegacyAbilitySourceArchive(db)
		require.NoError(t, err)
		assert.Equal(t, manifest, verified)
		assertLegacyAbilityArchiveEqual(t, db)
	})
	for _, tc := range []struct{ name, sql string }{
		{"archive_value_same_count", `ALTER TABLE "legacy_ability_sources" DISABLE TRIGGER USER; UPDATE "legacy_ability_sources" SET "tag"='tampered' WHERE "ordinal"=1; ALTER TABLE "legacy_ability_sources" ENABLE TRIGGER USER;`},
		{"archive_sequence", `ALTER TABLE "legacy_ability_sources" DISABLE TRIGGER USER; UPDATE "legacy_ability_sources" SET "ordinal"=99 WHERE "ordinal"=1; ALTER TABLE "legacy_ability_sources" ENABLE TRIGGER USER;`},
		{"manifest_hash", `ALTER TABLE "legacy_ability_source_manifests" DISABLE TRIGGER USER; UPDATE "legacy_ability_source_manifests" SET "sha256"=repeat('0',64); ALTER TABLE "legacy_ability_source_manifests" ENABLE TRIGGER USER;`},
		{"missing_seal", `ALTER TABLE "legacy_ability_source_manifests" DISABLE TRIGGER USER; DELETE FROM "legacy_ability_source_manifests"; ALTER TABLE "legacy_ability_source_manifests" ENABLE TRIGGER USER;`},
		{"disabled_protection", `ALTER TABLE "legacy_ability_sources" DISABLE TRIGGER USER`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := legacyAbilityArchiveDB(t, 2)
			_, err := ArchiveLegacyAbilitySource(db)
			require.NoError(t, err)
			before := legacyAbilitySourceState(t, db)
			require.NoError(t, db.Exec(tc.sql).Error)
			result, err := VerifyLegacyAbilitySourceArchive(db)
			assert.ErrorIs(t, err, ErrLegacyAbilitySource)
			assert.Nil(t, result)
			result, err = ArchiveLegacyAbilitySource(db)
			assert.ErrorIs(t, err, ErrLegacyAbilitySource)
			assert.Nil(t, result)
			assert.Equal(t, before, legacyAbilitySourceState(t, db))
		})
	}
}

func TestLegacyAbilitySourceTransactionsAndLocks(t *testing.T) {
	t.Run("outer_rollback_includes_schema_and_source_view", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 1)
		before := legacyAbilitySourceState(t, db)
		stop := errors.New("outer migration failed")
		err := db.Transaction(func(tx *gorm.DB) error {
			require.NoError(t, tx.Exec(`UPDATE "abilities" SET "tag"='outer-source-change'`).Error)
			_, err := ArchiveLegacyAbilitySource(tx)
			require.NoError(t, err)
			assertLegacyAbilityArchiveEqual(t, tx)
			return stop
		})
		assert.ErrorIs(t, err, stop)
		assert.Equal(t, before, legacyAbilitySourceState(t, db))
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySourceManifest{}))
		var functionExists bool
		require.NoError(t, db.Raw(`SELECT to_regprocedure('protect_legacy_ability_source()') IS NOT NULL`).Scan(&functionExists).Error)
		assert.False(t, functionExists)
		_, err = ArchiveLegacyAbilitySource(db)
		require.NoError(t, err)
	})
	t.Run("second_batch_SQL_failure_rolls_back_savepoint", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 251)
		before := legacyAbilitySourceState(t, db)
		require.NoError(t, db.Exec(`CREATE TABLE caller_progress (id bigint PRIMARY KEY)`).Error)
		var batches atomic.Int32
		const callback = "test:legacy_ability_source_batch_fault"
		require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.Table == "legacy_ability_sources" && batches.Add(1) == 2 {
				tx.AddError(tx.Exec(`SELECT 1/0`).Error)
			}
		}))
		t.Cleanup(func() { db.Callback().Create().Remove(callback) })
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			require.NoError(t, tx.Exec(`INSERT INTO caller_progress VALUES (1)`).Error)
			result, err := ArchiveLegacyAbilitySource(tx)
			assert.ErrorIs(t, err, ErrLegacyAbilitySource)
			assert.Nil(t, result)
			assert.False(t, tx.Migrator().HasTable(&LegacyAbilitySource{}))
			assert.False(t, tx.Migrator().HasTable(&LegacyAbilitySourceManifest{}))
			return tx.Exec(`INSERT INTO caller_progress VALUES (2)`).Error
		}))
		var progress int64
		require.NoError(t, db.Table("caller_progress").Count(&progress).Error)
		assert.EqualValues(t, 2, progress)
		assert.Equal(t, before, legacyAbilitySourceState(t, db))
		require.NoError(t, db.Callback().Create().Remove(callback))
		manifest, err := ArchiveLegacyAbilitySource(db)
		require.NoError(t, err)
		assert.EqualValues(t, 251, manifest.RowCount)
	})
	t.Run("source_lock_survives_inner_return", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 1)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(4)
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			_, err := ArchiveLegacyAbilitySource(tx)
			require.NoError(t, err)
			blocked := db.Transaction(func(other *gorm.DB) error {
				if err := other.Exec(`SET LOCAL lock_timeout='50ms'`).Error; err != nil {
					return err
				}
				return other.Exec(`UPDATE "abilities" SET "tag"='concurrent'`).Error
			})
			require.Error(t, blocked)
			assert.Contains(t, blocked.Error(), "55P03")
			assertLegacyAbilityArchiveEqual(t, tx)
			return nil
		}))
	})
	t.Run("cursor_close_failure_recovers_outer_transaction", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 1)
		before := legacyAbilitySourceState(t, db)
		require.NoError(t, db.Exec(`CREATE TABLE caller_progress (id bigint PRIMARY KEY)`).Error)
		const callback = "test:legacy_ability_source_close_fault"
		require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register(callback, func(tx *gorm.DB) {
			if tx.Statement.SQL.String() == "CLOSE legacy_ability_source_cursor" {
				// A real PostgreSQL error leaves the savepoint aborted until the
				// capture detects the close failure and rolls it back.
				tx.Statement.SQL.Reset()
				tx.Statement.SQL.WriteString("SELECT 1/0")
			}
		}))
		t.Cleanup(func() { db.Callback().Raw().Remove(callback) })
		require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
			result, err := ArchiveLegacyAbilitySource(tx)
			assert.ErrorIs(t, err, ErrLegacyAbilitySource)
			assert.Nil(t, result)
			return tx.Exec(`INSERT INTO caller_progress VALUES (1)`).Error
		}))
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySourceManifest{}))
		assert.Equal(t, before, legacyAbilitySourceState(t, db))
	})
	t.Run("concurrent_capture_has_one_seal", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 251)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(4)
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]*LegacyAbilitySourceManifest, 2)
		errorsFound := make([]error, 2)
		for i := range 2 {
			wg.Go(func() {
				<-start
				results[i], errorsFound[i] = ArchiveLegacyAbilitySource(db)
			})
		}
		close(start)
		wg.Wait()
		for _, err := range errorsFound {
			require.NoError(t, err)
		}
		assert.Equal(t, results[0], results[1])
		assertLegacyAbilityArchiveEqual(t, db)
	})
}

func TestLegacyAbilitySourceMissingInvalidAndEmpty(t *testing.T) {
	t.Run("missing_is_not_empty", func(t *testing.T) {
		db := UseTestDatabases(t)
		result, err := ArchiveLegacyAbilitySource(db)
		assert.ErrorIs(t, err, ErrLegacyAbilitySource)
		assert.Nil(t, result)
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
		result, err = VerifyLegacyAbilitySourceArchive(db)
		assert.ErrorIs(t, err, ErrLegacyAbilitySource)
		assert.Nil(t, result)
	})
	t.Run("explicit_empty_source", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 0)
		first, err := ArchiveLegacyAbilitySource(db)
		require.NoError(t, err)
		assert.Zero(t, first.RowCount)
		second, err := ArchiveLegacyAbilitySource(db)
		require.NoError(t, err)
		assert.Equal(t, first, second)
		verified, err := VerifyLegacyAbilitySourceArchive(db)
		require.NoError(t, err)
		assert.Equal(t, first, verified)
	})
	for _, tc := range []struct{ name, sql string }{
		{"missing_column", `ALTER TABLE "abilities" RENAME COLUMN "weight" TO "lost_weight"`},
		{"non_integer_weight", `ALTER TABLE "abilities" ALTER COLUMN "weight" DROP DEFAULT; ALTER TABLE "abilities" ALTER COLUMN "weight" TYPE numeric USING "weight"::numeric; UPDATE "abilities" SET "weight"=1.5`},
		{"integer_bool_must_not_be_inferred", `ALTER TABLE "abilities" ALTER COLUMN "enabled" TYPE integer USING CASE WHEN "enabled" THEN 1 ELSE 0 END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := legacyAbilityArchiveDB(t, 1)
			require.NoError(t, db.Exec(tc.sql).Error)
			result, err := ArchiveLegacyAbilitySource(db)
			assert.ErrorIs(t, err, ErrLegacyAbilitySource)
			assert.Nil(t, result)
			assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
		})
	}
	t.Run("read_error_is_not_empty_or_raw_source_error", func(t *testing.T) {
		db := legacyAbilityArchiveDB(t, 1)
		const canary = "source-value-must-not-escape"
		const callback = "test:legacy_ability_source_read_fault"
		require.NoError(t, db.Callback().Row().Before("gorm:row").Register(callback, func(tx *gorm.DB) {
			if strings.HasPrefix(tx.Statement.SQL.String(), "FETCH FORWARD") {
				tx.AddError(fmt.Errorf("source decode failed: %s", canary))
			}
		}))
		t.Cleanup(func() { db.Callback().Row().Remove(callback) })
		result, err := ArchiveLegacyAbilitySource(db)
		assert.ErrorIs(t, err, ErrLegacyAbilitySource)
		assert.Nil(t, result)
		assert.NotContains(t, err.Error(), canary)
		assert.False(t, db.Migrator().HasTable(&LegacyAbilitySource{}))
	})
	result, err := ArchiveLegacyAbilitySource(nil)
	assert.ErrorIs(t, err, ErrLegacyAbilitySource)
	assert.Nil(t, result)
}
