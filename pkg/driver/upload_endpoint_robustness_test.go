package driver

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestGetUploadEndpointRejectsNilOutput(t *testing.T) {
	if err := New().GetUploadEndpoint(nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("GetUploadEndpoint(nil) = %v, want ErrWrongParams", err)
	}
}

func TestGetUploadEndpointRejectsMalformedSuccessfulResponse(t *testing.T) {
	client := uploadEndpointClientForBody(`{"endpoint":"","gettokenurl":"https://token.invalid"}`)
	var endpoint UploadEndpointResp
	if err := client.GetUploadEndpoint(&endpoint); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("empty upload endpoint error = %v, want ErrUnexpected", err)
	}
}

func TestGetUploadEndpointRejectsHTTPFailureEvenWithUsableBody(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"endpoint":"https://oss.invalid","gettokenurl":"https://token.invalid"}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	var endpoint UploadEndpointResp
	if err := client.GetUploadEndpoint(&endpoint); !errors.Is(err, ErrUnexpected) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("GetUploadEndpoint HTTP failure = %v, want ErrUnexpected with status", err)
	}
}

func TestGetUploadEndpointPopulatesCallerValue(t *testing.T) {
	client := uploadEndpointClientForBody(`{"endpoint":"https://oss-cn-test.aliyuncs.com","gettokenurl":"https://token.invalid"}`)
	var endpoint UploadEndpointResp
	if err := client.GetUploadEndpoint(&endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint.Endpoint != "https://oss-cn-test.aliyuncs.com" || endpoint.GetTokenURL != "https://token.invalid" {
		t.Fatalf("unexpected upload endpoint: %#v", endpoint)
	}
}

func TestGetUploadEndpointSanitizesNetworkURL(t *testing.T) {
	transport := driverTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://uplb.115.com/3.0/getuploadinfo.php?token=secret", Err: errors.New("network down")}
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	var endpoint UploadEndpointResp
	err := client.GetUploadEndpoint(&endpoint)
	if err == nil || strings.Contains(err.Error(), "token=secret") {
		t.Fatalf("upload endpoint network error was not sanitized: %v", err)
	}
}

func TestSanitizeHTTPErrorRecursivelyRedactsNestedURLErrors(t *testing.T) {
	inner := &url.Error{Op: "dial", URL: "https://inner.example.invalid/path?inner_secret=one#fragment", Err: errors.New("network down")}
	outer := &url.Error{Op: "Get", URL: "https://outer.example.invalid/path?outer_secret=two", Err: inner}

	err := sanitizeHTTPError(outer)
	if err == nil {
		t.Fatal("sanitizeHTTPError returned nil")
	}
	text := err.Error()
	for _, secret := range []string{"inner_secret=one", "outer_secret=two", "#fragment"} {
		if strings.Contains(text, secret) {
			t.Fatalf("nested URL error leaked %q: %v", secret, err)
		}
	}
	if !strings.Contains(text, "network down") {
		t.Fatalf("sanitized nested error lost root cause: %v", err)
	}
}

func uploadEndpointClientForBody(body string) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	return New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
}
