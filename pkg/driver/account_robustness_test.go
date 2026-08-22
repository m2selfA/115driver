package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestCookieCheckPreservesNetworkFailureInsteadOfReportingBadCookie(t *testing.T) {
	transport := driverTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	err := client.CookieCheck()
	if err == nil || errors.Is(err, ErrBadCookie) || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("CookieCheck network failure = %v, want network error distinct from ErrBadCookie", err)
	}
}

func TestCookieCheckDoesNotMisclassifyHTTPFailureAsBadCookie(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":false,"detail":"secret-body"}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	err := client.CookieCheck()
	if !errors.Is(err, ErrUnexpected) || errors.Is(err, ErrBadCookie) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("CookieCheck HTTP failure = %v, want HTTP ErrUnexpected distinct from ErrBadCookie", err)
	}
	if strings.Contains(err.Error(), "secret-body") {
		t.Fatalf("CookieCheck HTTP status error leaked body: %v", err)
	}
}

func TestLoginCheckRejectsHTTPFailureBeforeZeroStateSuccess(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("upstream failed")),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	if err := client.LoginCheck(); !errors.Is(err, ErrUnexpected) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("LoginCheck HTTP failure = %v, want ErrUnexpected with status", err)
	}
	if client.UserID != 0 {
		t.Fatalf("LoginCheck HTTP failure mutated UserID to %d", client.UserID)
	}
}

func TestCookieCheckStillReportsExplicitLoggedOutState(t *testing.T) {
	client := accountClientForBody(`{"state":false}`)
	if err := client.CookieCheck(); !errors.Is(err, ErrBadCookie) {
		t.Fatalf("CookieCheck logged-out response = %v, want ErrBadCookie", err)
	}
}

func TestLoginCheckRejectsMalformedSuccessfulEmptyIdentity(t *testing.T) {
	client := accountClientForBody(`{"state":0,"data":{"user_id":0}}`)
	if err := client.LoginCheck(); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("LoginCheck malformed success = %v, want ErrUnexpected", err)
	}
	if client.UserID != 0 {
		t.Fatalf("LoginCheck malformed success mutated UserID to %d", client.UserID)
	}
}

func TestLoginCheckPublishesValidIdentity(t *testing.T) {
	client := accountClientForBody(`{"state":0,"data":{"user_id":123}}`)
	if err := client.LoginCheck(); err != nil {
		t.Fatal(err)
	}
	if client.UserID != 123 {
		t.Fatalf("LoginCheck UserID = %d, want 123", client.UserID)
	}
}

func TestGetUserRejectsMalformedSuccessfulEmptyIdentity(t *testing.T) {
	client := accountClientForBody(`{"state":true,"data":{}}`)
	user, err := client.GetUser()
	if user != nil || !errors.Is(err, ErrUnexpected) {
		t.Fatalf("GetUser malformed success = %#v, %v; want nil, ErrUnexpected", user, err)
	}
}

func TestGetUserReturnsValidIdentity(t *testing.T) {
	client := accountClientForBody(`{"state":true,"data":{"user_id":123,"user_name":"tester"}}`)
	user, err := client.GetUser()
	if err != nil {
		t.Fatal(err)
	}
	if user == nil || user.UserID != 123 || user.UserName != "tester" {
		t.Fatalf("unexpected user info: %#v", user)
	}
}

func accountClientForBody(body string) *Pan115Client {
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
