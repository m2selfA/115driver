package driver

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

type uploadEndpointFailureTransport struct {
	err error
}

func (transport uploadEndpointFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, transport.err
}

type uploadEndpointResponseTransport struct {
	body string
}

func (transport uploadEndpointResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(transport.body)),
		Request:    req,
	}, nil
}

func TestGetOSSEndpointDebugFallbackDoesNotLogRawError(t *testing.T) {
	const secret = "do-not-log-this-error-detail"
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{
		Transport: uploadEndpointFailureTransport{err: fmt.Errorf("simulated transport failure: %s", secret)},
	})))
	var logs bytes.Buffer
	client.debugWriter = &logs
	client.SetDebug(true)

	if got := client.getOSSEndpoint(true); got != OSSEndpoint {
		t.Fatalf("fallback endpoint = %q, want %q", got, OSSEndpoint)
	}
	logText := logs.String()
	if !strings.Contains(logText, "internal upload endpoint discovery failed; falling back to public endpoint error_type=") {
		t.Fatalf("missing safe fallback diagnostic: %q", logText)
	}
	if strings.Contains(logText, secret) {
		t.Fatalf("fallback diagnostic leaked raw error detail: %q", logText)
	}
}

func TestGetOSSEndpointDebugFallbackDoesNotLogUnsupportedEndpoint(t *testing.T) {
	const endpointSecret = "https://private-endpoint.example.invalid"
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{
		Transport: uploadEndpointResponseTransport{body: `{"endpoint":"` + endpointSecret + `"}`},
	})))
	var logs bytes.Buffer
	client.debugWriter = &logs
	client.SetDebug(true)

	if got := client.getOSSEndpoint(true); got != OSSEndpoint {
		t.Fatalf("fallback endpoint = %q, want %q", got, OSSEndpoint)
	}
	logText := logs.String()
	if !strings.Contains(logText, "internal upload endpoint is not Aliyun-compatible; falling back to public endpoint") {
		t.Fatalf("missing unsupported-endpoint fallback diagnostic: %q", logText)
	}
	if strings.Contains(logText, endpointSecret) {
		t.Fatalf("fallback diagnostic leaked endpoint value: %q", logText)
	}
}

func TestGetOSSEndpointWithoutInternalUploadDoesNotProbeOrLog(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{
		Transport: uploadEndpointFailureTransport{err: fmt.Errorf("request should not run")},
	})))
	var logs bytes.Buffer
	client.debugWriter = &logs
	client.SetDebug(true)

	if got := client.getOSSEndpoint(false); got != OSSEndpoint {
		t.Fatalf("public endpoint = %q, want %q", got, OSSEndpoint)
	}
	if logs.Len() != 0 {
		t.Fatalf("disabled internal upload unexpectedly emitted debug activity: %q", logs.String())
	}
}
