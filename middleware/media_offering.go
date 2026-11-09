package middleware

import "net/http"

// IsMediaGatewayEndpoint identifies synchronous image and audio HTTP ingress.
func IsMediaGatewayEndpoint(method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch path {
	case "/v1/images/generations", "/v1/images/edits", "/v1/audio/speech", "/v1/audio/transcriptions":
		return true
	default:
		return false
	}
}
