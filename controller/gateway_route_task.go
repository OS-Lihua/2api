package controller

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/capability"
	"github.com/QuantumNous/new-api/pkg/clock"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	taskplugin "github.com/QuantumNous/new-api/relay/channel/task/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

// gatewayPreparedTask owns only transient candidate inputs. Route chooses once;
// reserved preparation rebuilds the descriptor before the actual send.
type gatewayPreparedTask struct {
	Context       *gin.Context
	Info          *relaycommon.RelayInfo
	Adaptor       *taskplugin.TaskAdaptor
	Facts         map[string]any
	RetainedFacts map[string]any
	SaleInput     billingexpr.RequestInput
	RouteFacts    service.GatewayRouteFacts
	Plugin        *jsplugin.LoadedPlugin
}

func prepareGatewayTaskRoute(c *gin.Context, selection *service.GatewaySelection, plugin *jsplugin.LoadedPlugin, requested, requestID, publicTaskID string, started time.Time, origins []*model.Task) (*gatewayPreparedTask, error) {
	candidate := c.Copy()
	candidate.Request = c.Request.Clone(c.Request.Context())
	pluginKey, taskProtocol := strings.CutPrefix(string(selection.Channel.Protocol), "task:")
	if !taskProtocol {
		return nil, service.ErrGatewayConversion
	}
	if value, exists := candidate.Get(jsplugin.ContextKeyPinnedEndpoint); exists {
		pinned, ok := value.(jsplugin.PinnedEndpoint)
		if !ok {
			return nil, service.ErrGatewayConversion
		}
		matched := false
		for _, binding := range pinned.Candidates {
			if binding.Plugin.Meta.Key != pluginKey || binding.Model != selection.Channel.UpstreamModelName {
				continue
			}
			plugin, pinned.Plugin, pinned.Protocol, pinned.Operation = binding.Plugin, binding.Plugin, binding.Protocol, binding.Operation
			pinned.MappedModel = selection.Channel.UpstreamModelName
			matched = true
			break
		}
		if !matched {
			return nil, service.ErrGatewayConversion
		}
		candidate.Set(jsplugin.ContextKeyPinnedEndpoint, pinned)
		if value, exists := candidate.Get(jsplugin.ContextKeyProtocolRequest); exists {
			request, ok := value.(jsplugin.ProtocolRequestContext)
			if !ok {
				return nil, service.ErrGatewayConversion
			}
			request.Protocol, request.Operation, request.UpstreamModel = pinned.Protocol, pinned.Operation.Name, selection.Channel.UpstreamModelName
			candidate.Set(jsplugin.ContextKeyProtocolRequest, request)
		}
		candidate.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: pinned.Generation, Plugin: plugin})
	} else if value, exists := candidate.Get(jsplugin.ContextKeyPinnedRoute); exists {
		pinned, ok := value.(jsplugin.PinnedRoute)
		if !ok {
			return nil, service.ErrGatewayConversion
		}
		matched := false
		for _, binding := range pinned.Candidates {
			if binding.Plugin.Meta.Key != pluginKey {
				continue
			}
			plugin, pinned.Plugin, pinned.Route = binding.Plugin, binding.Plugin, binding.Route
			matched = true
			break
		}
		if !matched {
			return nil, service.ErrGatewayConversion
		}
		candidate.Set(jsplugin.ContextKeyPinnedRoute, pinned)
		candidate.Set(jsplugin.ContextKeyPinnedPlugin, jsplugin.PinnedPlugin{Generation: pinned.Generation, Plugin: plugin})
	} else if plugin.Meta.Key != pluginKey {
		return nil, service.ErrGatewayConversion
	}
	candidate.Set("expected_task_plugin_key", pluginKey)
	candidate.Set("platform", pluginKey)
	candidate.Set("task_plugin_key", pluginKey)
	candidate.Set("task_gateway_channel_selected", true)
	if err := middleware.SetupContextForGatewayCandidate(candidate, selection.Provider, requested); err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	candidate.Set("task_gateway_channel_selected", false)
	info := &relaycommon.RelayInfo{UserId: c.GetInt("id"), TokenId: c.GetInt("token_id"), UsingGroup: common.GetContextKeyString(c, constant.ContextKeyUserGroup), OriginModelName: requested, StartTime: started, RequestId: requestID, TaskRelayInfo: &relaycommon.TaskRelayInfo{PublicTaskID: publicTaskID}, ResponseFacts: &relaycommon.ResponseFacts{}}
	info.InitChannelMeta(candidate)
	var overrides map[string]any
	if err := common.UnmarshalJsonStr(selection.Channel.ParameterOverrides, &overrides); err != nil {
		return nil, service.ErrGatewayUnavailable
	}
	info.ParamOverride = overrides
	info.Action = c.GetString("task_action")
	info.UpstreamModelName = selection.Channel.UpstreamModelName
	if len(origins) > 0 {
		info.UpstreamModelName = origins[0].Properties.UpstreamModelName
		info.OriginTaskID = origins[0].GetUpstreamTaskID()
		for _, origin := range origins {
			info.OriginTasks = append(info.OriginTasks, relaycommon.OriginTaskRef{TaskID: origin.TaskID, UpstreamTaskID: origin.GetUpstreamTaskID(), Action: origin.Action, Status: string(origin.Status), Data: origin.Data})
		}
	}
	adaptor := taskplugin.New(plugin)
	adaptor.Init(info)
	if err := adaptor.DecodeRequestAndSetAction(candidate, info); err != nil {
		return nil, service.ErrGatewayCapability
	}
	var baselineFacts map[string]any
	var baselineErr error
	if len(overrides) > 0 {
		baselineFacts, baselineErr = adaptor.ExtractUsageFactsValidated(candidate, info)
		body, exists := candidate.Get("task_request")
		if !exists {
			value, _ := candidate.Get(jsplugin.ContextKeyRouteRequest)
			route, ok := value.(jsplugin.RouteRequestContext)
			if !ok {
				return nil, service.ErrGatewayCapability
			}
			body = route.RequestBody
		}
		encoded, err := common.Marshal(body)
		if err != nil {
			return nil, service.ErrGatewayCapability
		}
		if text, exists := candidate.Get(jsplugin.ContextKeyRequestBodyText); exists && plugin.Meta.PreservesJSONOrder() {
			if raw, ok := text.(json.RawMessage); ok {
				encoded = append([]byte(nil), raw...)
			}
		}
		encoded, err = relaycommon.ApplyParamOverrideWithRelayInfo(encoded, info)
		if err != nil {
			return nil, service.ErrGatewayCapability
		}
		var effective map[string]any
		if err := common.Unmarshal(encoded, &effective); err != nil {
			return nil, service.ErrGatewayCapability
		}
		if source, ok := body.(map[string]any); ok && effective["model"] != source["model"] {
			return nil, service.ErrGatewayCapability
		}
		candidate.Set("task_request", effective)
		candidate.Set(jsplugin.ContextKeyRequestBodyText, json.RawMessage(encoded))
	}
	required, err := adaptor.RequestCapabilities(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	if _, native := candidate.Get(jsplugin.ContextKeyPinnedRoute); native && slices.Contains(required.Capabilities.Features, capability.Streaming) && !slices.Contains(plugin.Meta.RequiredCapabilities, jsplugin.CapabilitySubmitSSEBusiness) {
		return nil, service.ErrGatewayCapability
	}
	if _, err := required.Capabilities.Narrow(selection.Capabilities); err != nil {
		return nil, service.ErrGatewayCapability
	}
	if maximum := selection.Capabilities.Limits.MaxImages; maximum != nil {
		hasImages := slices.Contains(required.Capabilities.InputModalities, capability.Image) || slices.Contains(required.Capabilities.OutputModalities, capability.Image)
		if required.ImageCount == nil && hasImages || required.ImageCount != nil && *required.ImageCount > *maximum {
			return nil, service.ErrGatewayCapability
		}
	}
	if maximum := selection.Capabilities.Limits.MaxDurationSeconds; maximum != nil {
		media := slices.Contains(required.Capabilities.InputModalities, capability.Video) || slices.Contains(required.Capabilities.InputModalities, capability.Audio) || slices.Contains(required.Capabilities.OutputModalities, capability.Video) || slices.Contains(required.Capabilities.OutputModalities, capability.Audio)
		if required.DurationSeconds == nil && media || required.DurationSeconds != nil && *required.DurationSeconds > *maximum {
			return nil, service.ErrGatewayCapability
		}
	}
	if maximum := selection.Capabilities.Limits.ContextLength; maximum != nil {
		if required.ContextText == nil || int64(tokenkit.Count(selection.Channel.UpstreamModelName, *required.ContextText)) > *maximum {
			return nil, service.ErrGatewayCapability
		}
	}
	facts, err := adaptor.ExtractUsageFactsValidated(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	retainedFacts := retainedGatewayTaskFacts(baselineFacts, facts, baselineErr, overrides)
	if len(origins) > 0 {
		merged := maps.Clone(origins[0].PrivateData.Gateway.Facts)
		maps.Copy(merged, facts)
		facts = merged
	}
	if facts == nil {
		facts = map[string]any{}
	}
	// The plugin owns quantity semantics; declared canonical units let the host
	// enforce Model/Channel limits without interpreting vendor request fields.
	schema, _ := plugin.Meta.UsageForModels(info.UpstreamModelName, info.OriginModelName)
	tokens, _, tokenErr := gatewayTaskTokens(schema, facts, true)
	if tokenErr != nil {
		return nil, service.ErrGatewayCapability
	}
	selection.InputTokens = int(tokens)
	info.RequestHeaders = map[string]string{}
	for name, values := range c.Request.Header {
		info.RequestHeaders[name] = strings.Join(values, ",")
	}
	saleInput, err := helper.ResolveTaskBillingRequestInput(candidate, info)
	if err != nil {
		return nil, service.ErrGatewayCapability
	}
	saleInput.Usage, saleInput.FreezeMoneyRequest = facts, true
	purchaseInput := billingexpr.RequestInput{Usage: facts}
	purchase, err := model.CurrentPriceVersion(candidate.Request.Context(), model.DB, model.PriceOwnerChannel, selection.Channel.ID, clock.Now().Unix())
	if err != nil {
		return nil, service.ErrGatewayPriceUnavailable
	}
	used, err := billingexpr.MoneyUsedVars(purchase.Expression)
	if err != nil {
		return nil, service.ErrGatewayPriceUnavailable
	}
	if used["param"] || used["header"] {
		purchaseInput, err = adaptor.PreviewBillingRequestInput(candidate, info)
		if err != nil {
			return nil, service.ErrGatewayCapability
		}
		purchaseInput.Usage = facts
	}
	return &gatewayPreparedTask{Plugin: plugin, Context: candidate, Info: info, Adaptor: adaptor, Facts: facts, RetainedFacts: retainedFacts, SaleInput: saleInput, RouteFacts: service.GatewayRouteFacts{SaleInput: saleInput, PurchaseInput: purchaseInput, MoneyInput: &billingexpr.MoneyInput{TaskUsage: true}}}, nil
}

// retainedGatewayTaskFacts keeps validated facts whose value the Channel's
// effective parameter overrides established. Comparing extraction before and
// after the override covers operations-format overrides without treating an
// unrelated top-level key as evidence for a client-supplied budget. These
// facts stay source quantities when a later completion report omits the field.
func retainedGatewayTaskFacts(baseline, effective map[string]any, baselineErr error, overrides map[string]any) map[string]any {
	if len(overrides) == 0 {
		return nil
	}
	var retained map[string]any
	for name, value := range effective {
		switch value.(type) {
		case string, bool, float64:
		default:
			continue
		}
		changed := baselineErr == nil && !reflect.DeepEqual(value, baseline[name])
		enforced := false
		if override, exists := overrides[name]; exists {
			enforced = fmt.Sprint(value) == fmt.Sprint(override)
		}
		if !changed && !enforced {
			continue
		}
		if retained == nil {
			retained = make(map[string]any)
		}
		retained[name] = value
	}
	return retained
}

// gatewayTaskTokens supplies the same canonical token quantities to Route TPM
// admission and its selected permit. Missing completion facts retain the estimate.
func gatewayTaskTokens(schema map[string]jsplugin.UsageFieldSchema, facts map[string]any, estimateMissing bool) (int64, bool, error) {
	total := decimal.Zero
	known := true
	for name, field := range schema {
		if field.Type != "number" || field.Unit != "token" {
			continue
		}
		value, exists := facts[name]
		if !exists {
			known = false
			if estimateMissing {
				total = total.Add(decimal.NewFromInt(1<<31 - 1))
			}
			continue
		}
		quantity, err := decimal.NewFromString(fmt.Sprint(value))
		if err != nil || quantity.IsNegative() {
			return 0, false, service.ErrGatewayCapability
		}
		total = total.Add(quantity)
	}
	total = total.Ceil()
	if total.GreaterThan(decimal.NewFromInt(1<<31 - 1)) {
		return 0, false, service.ErrGatewayCapability
	}
	return total.BigInt().Int64(), known, nil
}
