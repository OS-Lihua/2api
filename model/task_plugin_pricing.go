package model

import (
	"context"
	"errors"
	"io/fs"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/protocol"
	"github.com/QuantumNous/new-api/plugins"
	"gorm.io/gorm"
)

const (
	TaskPluginPricingReady         = "ready"
	TaskPluginPricingUnavailable   = "unavailable"
	TaskPluginPricingNotAuthorized = "not_authorized"
	TaskPluginPricingUnconfigured  = "unconfigured"
	TaskPluginPricingIncompatible  = "incompatible"
	TaskPluginPricingStale         = "stale"
)

type TaskPluginPricingIssue struct {
	OwnerType      string `json:"owner_type"`
	OwnerID        int    `json:"owner_id"`
	OwnerName      string `json:"owner_name"`
	PriceVersionID *int   `json:"price_version_id"`
	PluginKey      string `json:"plugin_key"`
	PluginName     string `json:"plugin_name"`
	Reason         string `json:"reason"`
}

type TaskPluginPricingDiagnostics struct {
	State  string                   `json:"state"`
	Issues []TaskPluginPricingIssue `json:"issues"`
}

// TaskPluginPricingVisibility is granted by the caller using current credentials,
// pricing:read and the corresponding owner permission. It is never client input.
type TaskPluginPricingVisibility struct {
	Offerings bool
	Channels  bool
}

// GetTaskPluginPricingDiagnostics reads only persisted owners and immutable
// current prices. The caller owns the snapshot transaction and authorization.
// A declaration in plugin metadata alone does not create a price owner.
func GetTaskPluginPricingDiagnostics(ctx context.Context, tx *gorm.DB, pluginKey string, visibility TaskPluginPricingVisibility) (*TaskPluginPricingDiagnostics, error) {
	result := &TaskPluginPricingDiagnostics{State: TaskPluginPricingNotAuthorized, Issues: []TaskPluginPricingIssue{}}
	if !visibility.Offerings && !visibility.Channels {
		return result, nil
	}
	if _, valid := protocol.Type("task:" + pluginKey).TaskPluginKey(); !valid {
		return nil, common.NewMessage("Invalid task plugin")
	}
	generation, at := jsplugin.DefaultRegistry.Generation(), clock.Now().Unix()
	tx = tx.WithContext(ctx)
	plugin, serving := generation.Get(pluginKey)
	if !serving {
		var err error
		plugin, err = installedTaskPluginPricingDeclaration(tx, generation, pluginKey)
		if err != nil {
			return nil, err
		}
		if plugin != nil {
			// A stopped declaration has no canonical index in the live generation.
			// Rebuild that index privately without publishing or enabling anything.
			declaration := jsplugin.NewRegistry()
			if err := declaration.ReplaceOverrides([]*jsplugin.LoadedPlugin{plugin}); err != nil {
				return nil, err
			}
			generation = declaration.Generation()
		}
	}
	pluginName := pluginKey
	if plugin != nil {
		pluginName = plugin.Meta.Name
	}

	// Stable model IDs and each Channel's actual upstream select its schema.
	var models []Model
	if err := tx.Find(&models).Error; err != nil {
		return nil, err
	}
	modelNames := make(map[int]string, len(models))
	for _, member := range models {
		modelNames[member.Id] = member.ModelName
	}
	type binding struct{ client, upstream string }
	bind := func(channel Channel) binding {
		return binding{client: modelNames[channel.ModelID], upstream: channel.UpstreamModelName}
	}
	check := func(ownerType string, ownerID int, name string, bindings []binding) error {
		if len(bindings) == 0 {
			return nil
		}
		price, err := CurrentPriceVersion(ctx, tx, ownerType, ownerID, at)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		issue := TaskPluginPricingIssue{OwnerType: ownerType, OwnerID: ownerID, OwnerName: name, PluginKey: pluginKey, PluginName: pluginName}
		if err == nil {
			issue.PriceVersionID = &price.ID
		}
		validated := map[string]bool{}
		for _, candidate := range bindings {
			client, upstream := candidate.client, candidate.upstream
			if canonical, ok := generation.CanonicalModel(client); ok {
				client = canonical
			}
			if canonical, ok := generation.CanonicalModel(upstream); ok {
				upstream = canonical
			}
			if plugin == nil || client == "" || (!slices.Contains(plugin.Meta.Models, client) && !slices.Contains(plugin.Meta.Models, upstream)) {
				issue.Reason = TaskPluginPricingStale
				break
			}
			if issue.PriceVersionID == nil {
				// Stopped plugins retain their installed declarations and prices;
				// switching one off does not demand new prices for dormant owners.
				if serving {
					issue.Reason = TaskPluginPricingUnconfigured
				}
				continue
			}
			schema, _ := plugin.Meta.UsageForModels(upstream, client)
			if err := validateTaskPriceSchema(price.Expression, schema, validated); err != nil {
				issue.Reason = TaskPluginPricingIncompatible
			}
		}
		if issue.Reason != "" {
			result.Issues = append(result.Issues, issue)
		}
		return nil
	}
	if visibility.Channels {
		var channels []Channel
		if err := tx.Where("protocol = ?", "task:"+pluginKey).Order("id").Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, channel := range channels {
			if err := check(PriceOwnerChannel, channel.ID, channel.UpstreamModelName, []binding{bind(channel)}); err != nil {
				return nil, err
			}
		}
	}
	if visibility.Offerings {
		var offerings []ModelOffering
		if err := tx.Order("id").Preload("Members").Find(&offerings).Error; err != nil {
			return nil, err
		}
		for _, offering := range offerings {
			ids := make([]int, 0, len(offering.Members))
			for _, member := range offering.Members {
				ids = append(ids, member.ModelID)
			}
			var channels []Channel
			if err := OfferingChannelsQuery(tx, &offering, ids).Order("id").Find(&channels).Error; err != nil {
				return nil, err
			}
			served := map[int]bool{}
			bindings := []binding{}
			for _, channel := range channels {
				served[channel.ModelID] = true
				if key, task := channel.Protocol.TaskPluginKey(); task && key == pluginKey {
					bindings = append(bindings, bind(channel))
				}
			}
			scope := offering.ChannelScope
			unrestricted := len(scope.IncludeProviderIDs)+len(scope.IncludeChannelIDs)+len(scope.ExcludeProviderIDs)+len(scope.ExcludeChannelIDs) == 0
			if unrestricted && plugin != nil {
				for _, member := range offering.Members {
					if served[member.ModelID] {
						continue
					}
					name := modelNames[member.ModelID]
					if canonical, ok := generation.CanonicalModel(name); ok {
						name = canonical
					}
					if slices.Contains(plugin.Meta.Models, name) {
						bindings = append(bindings, binding{client: name, upstream: name})
					}
				}
			}
			if err := check(PriceOwnerOffering, offering.ID, offering.Name, bindings); err != nil {
				return nil, err
			}
		}
	}
	result.State = TaskPluginPricingReady
	return result, nil
}

// A disabled installed version is not a deleted plugin. Resolve its declaration
// from this transaction, or the immutable embedded factory, never legacy prices
// or the Provider alias cache. Compilation errors make bindings stale, not free.
func installedTaskPluginPricingDeclaration(tx *gorm.DB, generation *jsplugin.RoutingGeneration, key string) (*jsplugin.LoadedPlugin, error) {
	var row TaskPlugin
	err := tx.Where(&TaskPlugin{Key: key, Active: true}).First(&row).Error
	source := ""
	if err == nil {
		source = string(row.Source)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	} else if generation.FactoryKey(key) {
		source, err = plugins.Source(key)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	if source == "" {
		return nil, nil
	}
	loaded, err := jsplugin.CompilePlugin(source, jsplugin.Options{Key: key})
	if err != nil {
		return nil, nil
	}
	return loaded, nil
}
