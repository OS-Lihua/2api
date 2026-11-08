package aws

import (
	"bytes"
	"errors"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// awsJSONResponseCapture keeps a bounded InvokeModel body across SDK decoding.
// A read error must not discard source counters received before that error.
type awsJSONResponseCapture struct {
	client   aws.HTTPClient
	response *http.Response
	readErr  error
}

func (capture *awsJSONResponseCapture) Do(request *http.Request) (*http.Response, error) {
	response, err := capture.client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	const limit = 64 << 20
	body, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if len(body) > limit {
		body = body[:limit]
		readErr = errors.New("AWS response exceeds capture limit")
	}
	copy := *response
	copy.Body = &awsJSONSourceBody{Reader: bytes.NewReader(body), readErr: readErr}
	capture.response, capture.readErr = &copy, readErr
	// The SDK may otherwise drop a complete JSON body on unexpected EOF.
	// The protocol handler receives the original read error after extraction.
	response.Body = io.NopCloser(bytes.NewReader(body))
	return response, nil
}

type awsJSONSourceBody struct {
	*bytes.Reader
	readErr error
}

func (body *awsJSONSourceBody) Read(target []byte) (int, error) {
	n, err := body.Reader.Read(target)
	if err == io.EOF && body.readErr != nil {
		err = body.readErr
	}
	return n, err
}
func (*awsJSONSourceBody) Close() error { return nil }
