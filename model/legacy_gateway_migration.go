package model

import (
	"context"
	"errors"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/money"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const legacyGatewayMigrationVersion = "legacy_gateway_v1"

var (
	ErrLegacyGatewayMigrationBlocked = errors.New("legacy gateway migration blocked; review the migration report")
	ErrLegacyGatewayMigrationFailed  = errors.New("legacy gateway migration failed; no domain changes committed")
	legacyGatewayMigrationMu         sync.Mutex
)

// LegacyGatewayMigrationReport contains counts, resource identities and fixed
// reasons only. Counts describe attempted work unless Committed is true.
type LegacyGatewayMigrationReport struct {
	Committed        bool                         `json:"committed"`
	AlreadyCompleted bool                         `json:"already_completed"`
	Counts           map[string]int64             `json:"counts"`
	Items            []LegacyGatewayMigrationItem `json:"items"`
}

type LegacyGatewayMigrationItem struct {
	Resource string `json:"resource"`
	ID       int64  `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Reason   string `json:"reason"`
}

type legacyGatewayMigration struct {
	Version     string `gorm:"type:varchar(64);primaryKey"`
	CompletedAt int64  `gorm:"not null"`
}

type legacyGatewayMigrationMapping struct {
	Resource   string `gorm:"type:varchar(32);primaryKey"`
	LegacyID   int64  `gorm:"primaryKey;autoIncrement:false"`
	LegacyName string `gorm:"type:varchar(256);primaryKey"`
	TargetID   int64  `gorm:"not null"`
}

func (report *LegacyGatewayMigrationReport) blocked(resource string, id int64, name, reason string) error {
	report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: resource, ID: id, Name: name, Reason: reason})
	return ErrLegacyGatewayMigrationBlocked
}

// MigrateLegacyGateway runs only in an offline, exclusive process after its CLI
// opens DB. Existing model mutation APIs use DB, so it is bound to the domain
// transaction here and restored even on panic. Do not call this from startup or
// a serving process. The completion marker and every domain write commit together.
func MigrateLegacyGateway(ctx context.Context) (*LegacyGatewayMigrationReport, error) {
	legacyGatewayMigrationMu.Lock()
	defer legacyGatewayMigrationMu.Unlock()
	report := &LegacyGatewayMigrationReport{Counts: map[string]int64{}, Items: []LegacyGatewayMigrationItem{}}
	if DB == nil || DB.Dialector.Name() != "postgres" {
		return report, report.blocked("migration", 0, "", "postgresql_required")
	}
	originalDB := DB
	stage := "schema"
	err := originalDB.WithContext(ctx).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(510045)`).Error; err != nil {
			return err
		}
		DB = tx
		defer func() { DB = originalDB }()
		if err := tx.AutoMigrate(&legacyGatewayMigration{}, &legacyGatewayMigrationMapping{}); err != nil {
			return err
		}
		var completed int64
		if err := tx.Model(&legacyGatewayMigration{}).Where("version = ?", legacyGatewayMigrationVersion).Count(&completed).Error; err != nil {
			return err
		}
		if completed != 0 {
			report.AlreadyCompleted = true
			return nil
		}
		if err := InitSettlementCurrency(); err != nil || SettlementCurrency() != money.Currency("USD") {
			return report.blocked("migration", 0, "", "usd_settlement_currency_required")
		}
		// Schema migration renames the old Provider table before creating the
		// new Channels. Key policies deliberately run after the final names exist.
		if err := migrateDB(); err != nil {
			return err
		}
		var foreignWallets int64
		if err := tx.Model(&Wallet{}).Where("account_type IN ? AND balance_currency <> ?", []LedgerAccountType{LedgerAccountUserWallet, LedgerAccountUserReceivable}, "USD").Count(&foreignWallets).Error; err != nil {
			return err
		}
		if foreignWallets != 0 {
			return report.blocked("wallet", 0, "", "usd_settlement_currency_required")
		}
		InitOptionMap()
		stage = "catalog"
		if err := migrateLegacyGatewayCatalog(ctx, tx, report); err != nil {
			return err
		}
		stage = "key_policy"
		if err := migrateLegacyGatewayKeyPolicies(tx, report); err != nil {
			return err
		}
		stage = "wallet"
		if err := migrateLegacyGatewayWallets(tx, report); err != nil {
			return err
		}
		stage = "completion"
		return tx.Create(&legacyGatewayMigration{Version: legacyGatewayMigrationVersion, CompletedAt: clock.Now().Unix()}).Error
	})
	if err != nil {
		if errors.Is(err, ErrLegacyGatewayMigrationBlocked) {
			return report, ErrLegacyGatewayMigrationBlocked
		}
		reason := stage + "_failed"
		name := ""
		var databaseError *pgconn.PgError
		if errors.As(err, &databaseError) {
			reason += "_sqlstate_" + databaseError.Code
			if databaseError.TableName != "" && databaseError.ColumnName != "" {
				name = databaseError.TableName + "." + databaseError.ColumnName
			}
		}
		report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "migration", Name: name, Reason: reason})
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		return report, ErrLegacyGatewayMigrationFailed
	}
	report.Committed = true
	return report, nil
}

type legacyGatewayOffering struct {
	providerIDs []int
	groups      []string
	aliases     []string
	model       Model
}

type legacyGatewayChannel struct {
	provider Provider
	name     string
	upstream string
	protocol protocol.Type
}

func migrateLegacyGatewayCatalog(ctx context.Context, tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	var providers []Provider
	// Source discovery needs only routing metadata, not credentials or URLs.
	if err := tx.Select("id", "type", "models", "group", "model_mapping", "param_override", "setting").Order("id").Find(&providers).Error; err != nil {
		return err
	}
	offerings := map[string]*legacyGatewayOffering{}
	var channels []legacyGatewayChannel
	aliasTargets := map[string]map[string]bool{}
	for _, provider := range providers {
		report.Counts["providers"]++
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "provider", LegacyID: int64(provider.Id), TargetID: int64(provider.Id)}).Error; err != nil {
			return err
		}
		mapping := map[string]string{}
		if provider.ModelMapping != nil && *provider.ModelMapping != "" {
			if err := common.UnmarshalJsonStr(*provider.ModelMapping, &mapping); err != nil || mapping == nil {
				return report.blocked("provider", int64(provider.Id), "", "invalid_model_mapping")
			}
		}
		for _, alias := range slices.Sorted(maps.Keys(mapping)) {
			target, cycle := followChannelModelMapping(mapping, alias)
			if cycle {
				return report.blocked("provider", int64(provider.Id), alias, "model_mapping_cycle")
			}
			if target != alias {
				if aliasTargets[alias] == nil {
					aliasTargets[alias] = map[string]bool{}
				}
				aliasTargets[alias][target] = true
			}
		}
		seen := map[string]bool{}
		for _, name := range provider.GetModels() {
			if seen[name] {
				continue
			}
			seen[name] = true
			// Exact spelling is the legacy public identity. Do not trim it or
			// use catalog prefix/suffix rules to infer a different Model.
			declaration := ModelOffering{Name: name, Members: []ModelOfferingMember{{ModelID: 1, Tier: 1}}}
			if err := declaration.normalize(); err != nil || declaration.Name != name {
				return report.blocked("provider", int64(provider.Id), name, "invalid_legacy_declaration")
			}
			entry := offerings[name]
			if entry == nil {
				entry = &legacyGatewayOffering{}
				offerings[name] = entry
			}
			entry.providerIDs = append(entry.providerIDs, provider.Id)
			entry.groups = append(entry.groups, provider.GetGroups()...)
			upstream, _ := followChannelModelMapping(mapping, name)
			if upstream != strings.TrimSpace(upstream) {
				return report.blocked("provider", int64(provider.Id), name, "invalid_upstream_model_name")
			}
			kind, err := legacyGatewayDefaultProtocol(provider)
			if err != nil {
				return report.blocked("provider", int64(provider.Id), name, "unverifiable_default_protocol")
			}
			channels = append(channels, legacyGatewayChannel{provider: provider, name: name, upstream: upstream, protocol: kind})
		}
	}
	for _, alias := range slices.Sorted(maps.Keys(aliasTargets)) {
		if offerings[alias] != nil {
			continue
		}
		targets := aliasTargets[alias]
		if len(targets) != 1 {
			return report.blocked("alias", 0, alias, "ambiguous_alias_target")
		}
		for target := range targets {
			if offering := offerings[target]; offering != nil {
				offering.aliases = append(offering.aliases, alias)
			}
		}
	}
	names := slices.Sorted(maps.Keys(offerings))
	if len(names) == 0 {
		return nil
	}
	for _, name := range names {
		entry := offerings[name]
		var existing []Model
		if err := tx.Where("model_name = ?", name).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 1 || len(existing) == 1 && existing[0].NameRule != NameRuleExact {
			return report.blocked("model", 0, name, "model_identity_conflict")
		}
		if len(existing) == 1 {
			entry.model = existing[0]
			report.Counts["models_reused"]++
		} else {
			entry.model = Model{ModelName: name, NameRule: NameRuleExact, Status: 1}
			if err := entry.model.Insert(); err != nil {
				return err
			}
			report.Counts["models_created"]++
		}
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "model", LegacyName: name, TargetID: int64(entry.model.Id)}).Error; err != nil {
			return err
		}
	}
	for _, source := range channels {
		model := offerings[source.name].model
		var existing []Channel
		if err := tx.Where("provider_id = ? AND protocol = ? AND upstream_model_name = ?", source.provider.Id, source.protocol, source.upstream).Find(&existing).Error; err != nil {
			return err
		}
		if len(existing) > 1 || len(existing) == 1 && existing[0].ModelID != model.Id {
			return report.blocked("provider", int64(source.provider.Id), source.name, "channel_identity_conflict")
		}
		var channel *Channel
		if len(existing) == 1 {
			channel = &existing[0]
			report.Counts["channels_reused"]++
			report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "channel", ID: int64(channel.ID), Name: source.name, Reason: "existing_configuration_preserved"})
		} else {
			overrides := "{}"
			if source.provider.ParamOverride != nil && *source.provider.ParamOverride != "" {
				overrides = *source.provider.ParamOverride
			}
			var err error
			channel, err = MutateChannel(ctx, 0, ChannelSave, func(record *Channel) error {
				*record = Channel{ProviderID: source.provider.Id, ModelID: model.Id, Protocol: source.protocol, UpstreamModelName: source.upstream, ParameterOverrides: overrides, Weight: 1}
				return nil
			})
			if err != nil {
				return report.blocked("provider", int64(source.provider.Id), source.name, "invalid_channel_declaration")
			}
			report.Counts["channels_created"]++
		}
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "channel", LegacyID: int64(source.provider.Id), LegacyName: source.name, TargetID: int64(channel.ID)}).Error; err != nil {
			return err
		}
	}
	// The alias view may belong to an earlier database in this process. Rebuild
	// it against this transaction before the existing conversion APIs use it.
	rebuildTaskAliasView()
	// Conversion must receive Configured: Effective loses the distinction
	// between a missing override and an explicit zero.
	snapshot, err := GetModelPricingSnapshot(names)
	if err != nil {
		return err
	}
	pricing := make(map[string]ModelPricingEntry, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		pricing[entry.ModelName] = entry
	}
	var actors []User
	if err := tx.Select("id").Order("role DESC, id").Limit(1).Find(&actors).Error; err != nil {
		return err
	}
	for _, name := range names {
		entry := offerings[name]
		slices.Sort(entry.providerIDs)
		slices.Sort(entry.groups)
		slices.Sort(entry.aliases)
		record := &ModelOffering{Name: name, Aliases: entry.aliases, Visibility: "groups", Groups: slices.Compact(entry.groups), Members: []ModelOfferingMember{{ModelID: entry.model.Id, Tier: 1}}, ChannelScope: OfferingChannelScope{IncludeProviderIDs: entry.providerIDs}, RouteMethod: "weight"}
		if err := record.normalize(); err != nil {
			return report.blocked("offering", 0, name, "invalid_offering_declaration")
		}
		var collisions int64
		if err := tx.Model(&ModelOfferingName{}).Where("name IN ?", append([]string{name}, entry.aliases...)).Count(&collisions).Error; err != nil {
			return err
		}
		var primaryCollisions int64
		if err := tx.Model(&ModelOffering{}).Where("name IN ?", append([]string{name}, entry.aliases...)).Count(&primaryCollisions).Error; err != nil {
			return err
		}
		if collisions != 0 || primaryCollisions != 0 {
			return report.blocked("offering", 0, name, "offering_name_conflict")
		}
		if err := SaveModelOffering(ctx, record, "save"); err != nil {
			return err
		}
		report.Counts["offerings_created"]++
		report.Counts["aliases_created"] += int64(len(record.Aliases))
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "offering", LegacyName: name, TargetID: int64(record.ID)}).Error; err != nil {
			return err
		}
		for _, alias := range record.Aliases {
			if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "alias", LegacyName: alias, TargetID: int64(record.ID)}).Error; err != nil {
				return err
			}
		}
		configured := pricing[name].Configured
		expression := ""
		if pricing[name].Effective["billing_setting.billing_mode"] == billing_setting.BillingModeTieredExpr {
			expression, _ = pricing[name].Effective["billing_setting.billing_expr"].(string)
		} else {
			converted, err := PreviewModelPricingConversion(name, configured)
			if err == nil && converted != nil && converted.UnsupportedReason == "" {
				expression = converted.Expression
			}
		}
		if expression == "" || len(actors) == 0 {
			report.Counts["offerings_disabled"]++
			reason := "sale_price_conversion_failed"
			if len(actors) == 0 {
				reason = "sale_price_creator_unavailable"
			}
			report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "offering", ID: int64(record.ID), Name: name, Reason: reason})
			continue
		}
		price := &PriceVersion{OwnerType: PriceOwnerOffering, OwnerID: record.ID, Currency: "USD", Expression: expression, EffectiveAt: clock.Now().Unix()}
		// Price creation has its own savepoint. Invalid old expressions leave
		// this new offering disabled without invalidating other conversions.
		if err := CreatePriceVersion(ctx, price, actors[0].Id); err != nil {
			report.Counts["offerings_disabled"]++
			report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "offering", ID: int64(record.ID), Name: name, Reason: "sale_price_validation_failed"})
			continue
		}
		if err := SaveModelOffering(ctx, record, "enable"); err != nil {
			return err
		}
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "sale_price", LegacyName: name, TargetID: int64(price.ID)}).Error; err != nil {
			return err
		}
		report.Counts["sale_prices_created"]++
		report.Counts["offerings_enabled"]++
	}
	return nil
}

// The Provider type supplies a conservative default. Names never select a
// protocol by resemblance; each new Channel still requires mapping confirmation.
func legacyGatewayDefaultProtocol(provider Provider) (protocol.Type, error) {
	if plugin, ok := jsplugin.DefaultRegistry.GetByChannelType(provider.Type); ok {
		return protocol.Type("task:" + plugin.Meta.Key), nil
	}
	switch provider.Type {
	case constant.ChannelTypeAnthropic, constant.ChannelTypeAws:
		return protocol.AnthropicMessages, nil
	case constant.ChannelTypeGemini, constant.ChannelTypeVertexAi:
		return protocol.GeminiGenerate, nil
	case constant.ChannelTypeCodex:
		return protocol.OpenAIResponses, nil
	case constant.ChannelTypeJina:
		return protocol.Rerank, nil
	case constant.ChannelTypeTaskPlugin:
		var settings dto.ProviderSettings
		if provider.Setting == nil || common.UnmarshalJsonStr(*provider.Setting, &settings) != nil {
			return "", ErrLegacyGatewayMigrationBlocked
		}
		kind := protocol.Type("task:" + settings.TaskPluginKey)
		if _, task := kind.TaskPluginKey(); !task {
			return "", ErrLegacyGatewayMigrationBlocked
		}
		return kind, nil
	default:
		if provider.Type <= 0 || provider.Type >= constant.ChannelTypeDummy {
			return "", ErrLegacyGatewayMigrationBlocked
		}
		return protocol.OpenAIChat, nil
	}
}

func migrateLegacyGatewayKeyPolicies(tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	var tokens []Token
	if err := lockForUpdate(tx).Select("id", "model_limits").Where(`"model_limits_enabled" = TRUE AND NOT EXISTS (SELECT 1 FROM "token_offering_policies" WHERE "token_id" = "tokens"."id")`).Order("id").Find(&tokens).Error; err != nil {
		return err
	}
	var names []ModelOfferingName
	if err := tx.Find(&names).Error; err != nil {
		return err
	}
	offeringIDs := make(map[string]int, len(names))
	for _, name := range names {
		offeringIDs[name.Name] = name.OfferingID
	}
	for _, token := range tokens {
		ids := map[int]bool{}
		unknown := map[string]bool{}
		for name := range strings.SplitSeq(token.ModelLimits, ",") {
			if id, known := offeringIDs[name]; known {
				ids[id] = true
			} else if name != "" && !unknown[name] {
				unknown[name] = true
				report.Items = append(report.Items, LegacyGatewayMigrationItem{Resource: "key", ID: int64(token.Id), Name: name, Reason: "unknown_model_limit_name"})
			}
		}
		if err := tx.Omit("Token").Create(&TokenOfferingPolicy{TokenID: token.Id, DefaultAllow: false}).Error; err != nil {
			return err
		}
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			if err := tx.Omit("Token", "Offering").Create(&TokenOfferingException{TokenID: token.Id, OfferingID: id}).Error; err != nil {
				return err
			}
		}
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "key_policy", LegacyID: int64(token.Id), TargetID: int64(token.Id)}).Error; err != nil {
			return err
		}
		report.Counts["key_policies_created"]++
	}
	return nil
}

func migrateLegacyGatewayWallets(tx *gorm.DB, report *LegacyGatewayMigrationReport) error {
	var users []struct {
		ID    int
		Quota int64
	}
	// Include soft-deleted users: deleting an identity does not erase its money.
	if err := lockForUpdate(tx).Table("users").Select("id", "quota").Order("id").Find(&users).Error; err != nil {
		return err
	}
	for _, user := range users {
		if user.Quota < 0 {
			return report.blocked("user", int64(user.ID), "", "negative_legacy_balance")
		}
		value, err := money.NewRationalValue(big.NewRat(user.Quota, 500000), "USD")
		if err != nil {
			return report.blocked("user", int64(user.ID), "", "legacy_balance_overflow")
		}
		amount, err := value.Credit()
		if err != nil {
			return report.blocked("user", int64(user.ID), "", "legacy_balance_overflow")
		}
		if amount.Nanos == 0 {
			report.Counts["zero_balances_skipped"]++
			continue
		}
		key := legacyGatewayMigrationVersion + ":user:" + strconv.Itoa(user.ID) + ":opening_balance"
		var existing []LedgerEntry
		if err := tx.Where("idempotency_key = ?", key).Find(&existing).Error; err != nil {
			return err
		}
		var entryID int64
		if len(existing) > 0 {
			entry := existing[0]
			var wallet Wallet
			if err := tx.First(&wallet, entry.WalletId).Error; err != nil {
				return err
			}
			if len(existing) != 1 || entry.AccountType != LedgerAccountUserWallet || entry.Type != LedgerEntryOpeningBalance || entry.Amount != amount || wallet.AccountType != LedgerAccountUserWallet || wallet.OwnerId != user.ID || wallet.Balance.Currency != "USD" {
				return report.blocked("user", int64(user.ID), "", "opening_balance_identity_conflict")
			}
			entryID = entry.Id
			report.Counts["opening_balances_reused"]++
		} else {
			entries, err := PostLedgerEntries(LedgerPosting{Account: UserWalletAccount(user.ID), Type: LedgerEntryOpeningBalance, Amount: amount, IdempotencyKey: key, Remark: "Legacy quota opening balance"})
			if err != nil {
				reason := "opening_balance_post_failed"
				if errors.Is(err, money.ErrOverflow) {
					reason = "legacy_balance_overflow"
				}
				return report.blocked("user", int64(user.ID), "", reason)
			}
			entryID = entries[0].Id
			report.Counts["opening_balances_created"]++
		}
		if err := tx.Create(&legacyGatewayMigrationMapping{Resource: "opening_balance", LegacyID: int64(user.ID), TargetID: entryID}).Error; err != nil {
			return err
		}
	}
	return nil
}
