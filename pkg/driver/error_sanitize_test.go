package driver

import (
	stderrors "errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

type noResultError struct{}

func (noResultError) Err(...string) error { return nil }

func TestCheckErrSanitizesNetworkURLSecrets(t *testing.T) {
	cause := stderrors.New("dial failed")
	input := &url.Error{
		Op:  "Get",
		URL: "https://user:password@example.invalid/share/down?receive_code=top-secret&token=signed-secret#fragment-secret",
		Err: cause,
	}
	got := CheckErr(input, noResultError{}, nil)
	if got == nil {
		t.Fatal("CheckErr returned nil")
	}
	text := got.Error()
	for _, secret := range []string{"top-secret", "signed-secret", "fragment-secret", "password"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sanitized network error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "example.invalid/share/down") {
		t.Fatalf("sanitized network error lost useful endpoint context: %s", text)
	}
	if !stderrors.Is(got, cause) {
		t.Fatalf("sanitized network error lost unwrap chain: %v", got)
	}
}

func TestCheckErrRejectsNon2xxBeforeEnvelope(t *testing.T) {
	resp := &resty.Response{RawResponse: &http.Response{StatusCode: http.StatusServiceUnavailable}}
	login := &LoginResp{State: 0}
	err := CheckErr(nil, login, resp)
	if !stderrors.Is(err, ErrUnexpected) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("CheckErr HTTP failure = %v, want ErrUnexpected with status", err)
	}
}

func TestShareAPINetworkErrorsDoNotLeakReceiveCode(t *testing.T) {
	const receiveCode = "top-secret-receive-code"
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.URL.Query().Get("receive_code"); got != receiveCode {
			t.Fatalf("request receive_code = %q, want %q", got, receiveCode)
		}
		return nil, stderrors.New("network down")
	})
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))

	for name, call := range map[string]func() error{
		"share-snap": func() error {
			_, err := client.GetShareSnap("share-code", receiveCode, "0")
			return err
		},
		"share-download": func() error {
			_, err := client.DownloadByShareCode("share-code", receiveCode, "file-id")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("network failure unexpectedly returned nil")
			}
			text := err.Error()
			if strings.Contains(text, receiveCode) || strings.Contains(text, "receive_code=") {
				t.Fatalf("share API network error leaked receive code: %s", text)
			}
		})
	}
}
