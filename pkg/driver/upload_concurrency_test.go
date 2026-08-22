package driver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

func TestUploadAvailableInitializesMetadataOnceAcrossConcurrentWorkers(t *testing.T) {
	var calls atomic.Int32
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(5 * time.Millisecond)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":true,"user_id":42,"userkey":"upload-key","size_limit":123456,"upload_allowed":true}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))

	const workers = 32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ok, err := client.UploadAvailable()
			if err != nil {
				errs <- err
				return
			}
			if !ok {
				errs <- ErrUnexpected
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("UploadAvailable failed: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upload-info request count = %d, want 1", got)
	}
	if client.UserID != 42 || client.Userkey != "upload-key" || client.UploadMetaInfo == nil || client.UploadMetaInfo.SizeLimit != 123456 {
		t.Fatalf("upload metadata not published consistently: user=%d key=%q meta=%#v", client.UserID, client.Userkey, client.UploadMetaInfo)
	}
}

func TestUploadAvailableRefreshesIncompletePreseededMetadata(t *testing.T) {
	var calls atomic.Int32
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":true,"user_id":7,"userkey":"fresh-key","size_limit":99,"upload_allowed":true}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	client.UserID = 7
	client.Userkey = "preseeded-key"
	client.UploadMetaInfo = nil

	ok, err := client.UploadAvailable()
	if err != nil || !ok {
		t.Fatalf("UploadAvailable = %v, %v", ok, err)
	}
	if calls.Load() != 1 || client.UploadMetaInfo == nil || client.Userkey != "fresh-key" {
		t.Fatalf("incomplete preseeded metadata was not refreshed: calls=%d key=%q meta=%#v", calls.Load(), client.Userkey, client.UploadMetaInfo)
	}
}

func TestUploadAvailableRejectsUploadMetadataFromDifferentAccount(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":true,"user_id":8,"userkey":"wrong-account-key","size_limit":99,"upload_allowed":true}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	client.UserID = 7

	ok, err := client.UploadAvailable()
	if err == nil || ok || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("UploadAvailable = %v, %v, want account mismatch", ok, err)
	}
	if client.UserID != 7 || client.Userkey != "" || client.UploadMetaInfo != nil {
		t.Fatalf("mismatched upload metadata was partially published: user=%d key=%q meta=%#v", client.UserID, client.Userkey, client.UploadMetaInfo)
	}
}

func TestUploadAvailableRejectsMalformedSuccessWithoutPublishingState(t *testing.T) {
	for name, body := range map[string]string{
		"missing-user-id": `{"state":true,"userkey":"upload-key","size_limit":99,"upload_allowed":true}`,
		"missing-userkey": `{"state":true,"user_id":7,"size_limit":99,"upload_allowed":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			})
			client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))

			ok, err := client.UploadAvailable()
			if err == nil || ok || !strings.Contains(err.Error(), "upload metadata response") {
				t.Fatalf("UploadAvailable = %v, %v, want malformed metadata error", ok, err)
			}
			if client.UserID != 0 || client.Userkey != "" || client.UploadMetaInfo != nil {
				t.Fatalf("malformed upload metadata was partially published: user=%d key=%q meta=%#v", client.UserID, client.Userkey, client.UploadMetaInfo)
			}
		})
	}
}

func TestUploadAvailableHonorsUploadAllowed(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":true,"user_id":7,"userkey":"upload-key","size_limit":99,"upload_allowed":false,"upload_allowed_msg":"account upload disabled"}`)),
			Request:    req,
		}, nil
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))

	ok, err := client.UploadAvailable()
	if ok || !errors.Is(err, ErrUploadNotAllowed) || !strings.Contains(err.Error(), "account upload disabled") {
		t.Fatalf("UploadAvailable = %v, %v, want ErrUploadNotAllowed with server reason", ok, err)
	}
	if client.UploadMetaInfo == nil || client.UploadMetaInfo.UploadAllowed {
		t.Fatalf("upload permission metadata not retained: %#v", client.UploadMetaInfo)
	}

	ok, err = client.UploadAvailable()
	if ok || !errors.Is(err, ErrUploadNotAllowed) {
		t.Fatalf("cached UploadAvailable = %v, %v, want ErrUploadNotAllowed", ok, err)
	}
}
