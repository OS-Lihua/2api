package aws

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/QuantumNous/new-api/relay/channel"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
)

func mantleRequestURL(info *relaycommon.RelayInfo) (string, error) {
	return mantleProviderRequestURL(channel.NewProviderMetadata(info))
}

func mantleProviderRequestURL(info *channel.ProviderRequest) (string, error) {
	parts := strings.Split(info.ApiKey, "|")
	if len(parts) != 2 && len(parts) != 3 {
		return "", fmt.Errorf("invalid AWS credentials: use APIKey|Region or AccessKey|SecretAccessKey|Region")
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return "", fmt.Errorf("invalid AWS credentials: empty credential or region")
		}
	}
	region := parts[len(parts)-1]
	for _, ch := range region {
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
			return "", fmt.Errorf("invalid AWS region")
		}
	}
	baseURL := strings.TrimRight(info.ChannelBaseUrl, "/")
	if baseURL == "" {
		baseURL = "https://bedrock-mantle." + region + ".api.aws"
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", fmt.Errorf("AWS Mantle base URL must be an HTTPS URL without credentials, query, or fragment")
	}
	switch {
	case strings.HasSuffix(endpoint.Path, "/anthropic/v1/messages"):
	case strings.HasSuffix(endpoint.Path, "/anthropic/v1"):
		endpoint.Path += "/messages"
	case strings.HasSuffix(endpoint.Path, "/anthropic"):
		endpoint.Path += "/v1/messages"
	default:
		endpoint.Path += "/anthropic/v1/messages"
	}
	return endpoint.String(), nil
}

// Mantle uses native Messages JSON and SSE, not the InvokeModel envelope.
// Sign the final payload and headers, including administrator overrides.
func (a *Protocol) newMantleRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (*http.Request, error) {
	if a.IsNova {
		return nil, fmt.Errorf("AWS Mantle Messages API requires a Claude model; use InvokeModel for Amazon Nova")
	}
	metadata := channel.NewProviderMetadata(info)
	overrides, err := channel.ResolveHeaderOverride(info, c)
	if err != nil {
		return nil, err
	}
	metadata.RequestHeaders = make(http.Header, len(overrides))
	for key, value := range overrides {
		metadata.RequestHeaders.Set(key, value)
	}
	return (&Provider{}).NewMantleRequest(c, metadata, requestBody)
}

func (p *Provider) NewMantleRequest(c *gin.Context, metadata channel.ProviderMetadata, requestBody io.Reader) (*http.Request, error) {
	info := metadata.ProviderRequest()
	endpoint, err := mantleProviderRequestURL(info)
	if err != nil {
		return nil, err
	}
	if isNovaModel(getAwsModelID(info.UpstreamModelName)) {
		return nil, fmt.Errorf("AWS Mantle Messages API requires a Claude model; use InvokeModel for Amazon Nova")
	}
	payload, err := io.ReadAll(requestBody)
	if err != nil {
		return nil, fmt.Errorf("decode AWS Mantle request: %w", err)
	}
	if !ojson.ValidBytes(payload) {
		return nil, fmt.Errorf("decode AWS Mantle request: invalid JSON")
	}
	if !ojson.GetBytesNoCopy(payload, "@this").IsObject() {
		return nil, fmt.Errorf("AWS Mantle request must be a JSON object")
	}
	// Use the channel's resolved model even for passthrough requests. Do not
	// apply InvokeModel's version suffixes or automatic cross-region prefixes.
	model := info.UpstreamModelName
	if model == "claude-haiku-4-5-20251001" {
		model = "claude-haiku-4-5"
	}
	if strings.HasPrefix(model, "claude-") {
		model = "anthropic." + model
	}
	doc := ojson.NewDoc(payload)
	for _, key := range []string{"anthropic_version", "anthropic_beta"} {
		if err := relaycommon.DeleteJSONMember(doc, key); err != nil {
			return nil, fmt.Errorf("marshal AWS Mantle request: %w", err)
		}
	}
	if err := relaycommon.SetJSONMember(doc, "", "model", model); err != nil {
		return nil, fmt.Errorf("marshal AWS Mantle request: %w", err)
	}
	body := doc.Bytes()
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create AWS Mantle request: %w", err)
	}
	if err := p.SetupHeader(c, &req.Header, metadata); err != nil {
		return nil, err
	}
	for key, values := range info.RequestHeaders {
		req.Header[key] = append([]string(nil), values...)
		if strings.EqualFold(key, "Host") {
			req.Host = info.RequestHeaders.Get(key)
		}
	}
	if err := p.SignRequest(req, metadata, body); err != nil {
		return nil, err
	}
	return req, nil
}
