package setting

import (
	"os"
	"strconv"

	"github.com/QuantumNous/new-api/common"
)

// GatewayBudgetDefaults supplies deployment defaults; persisted options take precedence.
// Durations are whole seconds. Each send also caps its deadline by the remaining total.
func GatewayBudgetDefaults() map[string]int {
	values := map[string]int{
		"GatewayMaxAttempts":             3,
		"GatewayTotalFirstByteSeconds":   90,
		"GatewayAttemptFirstByteSeconds": 30,
	}
	for key, env := range map[string]string{
		"GatewayMaxAttempts":             "GATEWAY_MAX_ATTEMPTS",
		"GatewayTotalFirstByteSeconds":   "GATEWAY_TOTAL_FIRST_BYTE_SECONDS",
		"GatewayAttemptFirstByteSeconds": "GATEWAY_ATTEMPT_FIRST_BYTE_SECONDS",
	} {
		raw := os.Getenv(env)
		if n, err := strconv.Atoi(raw); err == nil && ValidateGatewayBudgetOption(key, raw) == nil {
			values[key] = n
		}
	}
	return values
}

func IsGatewayBudgetOption(key string) bool {
	return key == "GatewayMaxAttempts" || key == "GatewayTotalFirstByteSeconds" || key == "GatewayAttemptFirstByteSeconds"
}

func ValidateGatewayBudgetOption(key, value string) error {
	var lower, upper int
	var message string
	switch key {
	case "GatewayMaxAttempts":
		lower, upper, message = 1, 5, "Maximum attempts must be an integer between 1 and 5"
	case "GatewayTotalFirstByteSeconds":
		lower, upper, message = 5, 600, "Total first-byte budget must be an integer between 5 and 600 seconds"
	case "GatewayAttemptFirstByteSeconds":
		lower, upper, message = 1, 600, "Attempt first-byte budget must be an integer between 1 and 600 seconds"
	default:
		return nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < lower || n > upper {
		return common.NewMessage(message)
	}
	return nil
}
