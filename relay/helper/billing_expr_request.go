package helper

import (
	"fmt"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

func ResolveIncomingBillingExprRequestInput(c *gin.Context, info *relaycommon.RelayInfo) (billingexpr.RequestInput, error) {
	if info != nil && info.BillingRequestInput != nil {
		input := cloneRequestInput(*info.BillingRequestInput)
		merged := cloneStringMap(info.RequestHeaders)
		maps.Copy(merged, input.Headers)
		input.Headers = merged
		return input, nil
	}

	input := billingexpr.RequestInput{}
	if info != nil {
		input.Headers = cloneStringMap(info.RequestHeaders)
	}

	bodyBytes, err := readIncomingBillingExprBody(c)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	return input, nil
}

// ResolveImageBillingRequestInput freezes only the validated scalar image
// parameters needed by pricing. Image files, prompts and base64 payloads are
// deliberately excluded, including for multipart edits.
func ResolveImageBillingRequestInput(c *gin.Context, info *relaycommon.RelayInfo, input billingexpr.RequestInput) (billingexpr.RequestInput, error) {
	request, ok := info.Request.(*dto.ImageRequest)
	if !ok {
		return input, nil
	}
	count, err := request.ImageCount(false)
	if err != nil {
		return input, err
	}
	body := map[string]any{"model": request.Model, "n": count, "size": request.Size, "quality": request.Quality}
	if request.BillingParameters != nil {
		body["parameters"] = request.BillingParameters
	}
	encoded, err := common.Marshal(body)
	if err != nil {
		return input, err
	}
	input.Body = encoded
	input.ImageCount = &count
	return input, nil
}

func BuildBillingExprRequestInputFromRequest(request dto.Request, headers map[string]string) (billingexpr.RequestInput, error) {
	input := billingexpr.RequestInput{
		Headers: cloneStringMap(headers),
	}
	if request == nil {
		return input, nil
	}

	bodyBytes, err := common.Marshal(request)
	if err != nil {
		return billingexpr.RequestInput{}, err
	}
	input.Body = bodyBytes
	return input, nil
}

// ResolveTaskBillingRequestInput preserves original scalar task parameters,
// including multipart fields, without putting file contents in pricing inputs.
func ResolveTaskBillingRequestInput(c *gin.Context, info *relaycommon.RelayInfo) (billingexpr.RequestInput, error) {
	input, err := ResolveIncomingBillingExprRequestInput(c, info)
	if err != nil || !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "multipart/") {
		return input, err
	}
	form, err := common.ParseMultipartFormReusable(c)
	if err != nil {
		return input, err
	}
	input.Body, err = taskPricingParameters(form.Value)
	return input, err
}

func taskPricingParameters(fields map[string][]string) ([]byte, error) {
	values := make(map[string]any, len(fields))
	for name, entries := range fields {
		if len(entries) == 1 {
			values[name] = entries[0]
		} else {
			values[name] = entries
		}
	}
	return common.Marshal(values)
}

// BuildTaskBillingRequestInput inspects the prepared wire request without
// consuming its body. Multipart files are excluded from pricing parameters.
// The returned context is transient; durable tasks retain boolean decisions.
func BuildTaskBillingRequestInput(request *http.Request) (billingexpr.RequestInput, error) {
	input := billingexpr.RequestInput{Headers: make(map[string]string, len(request.Header))}
	for name, values := range request.Header {
		input.Headers[name] = strings.Join(values, ",")
	}
	if request.Body == nil || request.Body == http.NoBody {
		return input, nil
	}
	if request.GetBody == nil {
		return input, fmt.Errorf("task pricing requires a replayable request body")
	}
	body, err := request.GetBody()
	if err != nil {
		return input, err
	}
	defer body.Close()
	mediaType := "application/json"
	var parameters map[string]string
	if contentType := request.Header.Get("Content-Type"); strings.TrimSpace(contentType) != "" {
		mediaType, parameters, err = mime.ParseMediaType(contentType)
		if err != nil {
			return input, err
		}
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, parameters["boundary"])
		fields := make(map[string][]string)
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return input, err
			}
			if part.FileName() != "" || part.FormName() == "" {
				continue
			}
			value, err := io.ReadAll(part)
			if err != nil {
				return input, err
			}
			fields[part.FormName()] = append(fields[part.FormName()], string(value))
		}
		input.Body, err = taskPricingParameters(fields)
		return input, err
	}
	if isJSONContentType(mediaType) {
		input.Body, err = io.ReadAll(body)
	}
	return input, err
}

func readIncomingBillingExprBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || !isJSONContentType(c.Request.Header.Get("Content-Type")) {
		return nil, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	return storage.Bytes()
}

func cloneRequestInput(src billingexpr.RequestInput) billingexpr.RequestInput {
	input := billingexpr.RequestInput{
		Headers: cloneStringMap(src.Headers),
	}
	if src.ImageCount != nil {
		count := *src.ImageCount
		input.ImageCount = &count
	}
	if len(src.Body) > 0 {
		input.Body = append([]byte(nil), src.Body...)
	}
	return input
}

func isJSONContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "application/json")
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		if strings.TrimSpace(key) == "" {
			continue
		}
		dst[key] = value
	}
	return dst
}
