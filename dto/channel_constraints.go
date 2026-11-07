package dto

type ProviderPinSource string

const (
	PinSourceToken      ProviderPinSource = "token"       // Rank 0, highest
	PinSourceOriginTask ProviderPinSource = "origin_task" // Rank 10
)

const (
	PinRankToken      = 0
	PinRankOriginTask = 10
)

type PinRetryMode int

const (
	PinRetrySameChannel PinRetryMode = iota
	PinRetrySingleAttempt
)

func (m PinRetryMode) Stricter(other PinRetryMode) PinRetryMode {
	if m == PinRetrySingleAttempt || other == PinRetrySingleAttempt {
		return PinRetrySingleAttempt
	}
	return PinRetrySameChannel
}

type ProviderPin struct {
	ChannelId int
	Source    ProviderPinSource
	Rank      int
	RetryMode PinRetryMode
}

type ProviderFilterKind string

const (
	FilterRequestPath        ProviderFilterKind = "request_path"
	FilterTaskPluginEndpoint ProviderFilterKind = "task_plugin_endpoint"
	FilterTaskPluginIdentity ProviderFilterKind = "task_plugin_identity"
	FilterResponsesWebSocket ProviderFilterKind = "responses_websocket"
)

type ProviderFilter struct {
	Kind                   ProviderFilterKind
	RequestPath            string
	TaskPluginKey          string
	TaskPluginKeys         []string
	TaskPluginChannelTypes []int
	// TaskPluginRelayOnly excludes endpoint candidates after an ordinary relay
	// channel wins selection, so retries keep using the same protocol handler.
	TaskPluginRelayOnly bool
}

type ProviderConstraints struct {
	Pins    []ProviderPin
	Filters []ProviderFilter
}

func (cc *ProviderConstraints) AddPin(p ProviderPin) {
	if cc == nil {
		return
	}
	cc.Pins = append(cc.Pins, p)
}

func (cc *ProviderConstraints) AddFilter(f ProviderFilter) {
	if cc == nil {
		return
	}
	cc.Filters = append(cc.Filters, f)
}

// ResolvedPin returns the winning pin after priority resolution.
// Lowest Rank wins. Pins that name the same channel are merged (stricter RetryMode).
// overridden lists pins that lost to a different channel (for warn logging).
func (cc *ProviderConstraints) ResolvedPin() (ProviderPin, bool, []ProviderPin) {
	if cc == nil || len(cc.Pins) == 0 {
		return ProviderPin{}, false, nil
	}

	merged := make(map[int]ProviderPin, len(cc.Pins))
	order := make([]int, 0, len(cc.Pins))
	for _, pin := range cc.Pins {
		existing, seen := merged[pin.ChannelId]
		if !seen {
			merged[pin.ChannelId] = pin
			order = append(order, pin.ChannelId)
			continue
		}
		existing.RetryMode = existing.RetryMode.Stricter(pin.RetryMode)
		if pin.Rank < existing.Rank {
			existing.Rank = pin.Rank
			existing.Source = pin.Source
		}
		merged[pin.ChannelId] = existing
	}

	winner := merged[order[0]]
	for _, channelID := range order[1:] {
		candidate := merged[channelID]
		if candidate.Rank < winner.Rank {
			winner = candidate
		}
	}

	var overridden []ProviderPin
	for _, channelID := range order {
		candidate := merged[channelID]
		if candidate.ChannelId != winner.ChannelId {
			overridden = append(overridden, candidate)
		}
	}
	return winner, true, overridden
}

func (cc *ProviderConstraints) SuppressesRetry() bool {
	pin, found, _ := cc.ResolvedPin()
	return found && pin.RetryMode == PinRetrySingleAttempt
}
