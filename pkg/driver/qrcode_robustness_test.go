package driver

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestQRCodeSessionNilMethodsReturnWrongParams(t *testing.T) {
	var session *QRCodeSession
	if _, err := session.QRCode(); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCode nil-session error = %v, want ErrWrongParams", err)
	}
	if _, err := session.QRCodeByApi(); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeByApi nil-session error = %v, want ErrWrongParams", err)
	}

	client := New()
	if _, err := client.QRCodeLogin(session); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeLogin nil-session error = %v, want ErrWrongParams", err)
	}
	if _, err := client.QRCodeLoginWithApp(session, LoginAppAndroid); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeLoginWithApp nil-session error = %v, want ErrWrongParams", err)
	}
	if _, err := client.QRCodeStatus(session); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeStatus nil-session error = %v, want ErrWrongParams", err)
	}
}

func TestQRCodeStatusNilPredicatesAreFalse(t *testing.T) {
	var status *QRCodeStatus
	if status.IsWaiting() || status.IsScanned() || status.IsAllowed() || status.IsExpired() || status.IsCanceled() {
		t.Fatal("nil QRCodeStatus unexpectedly matched a status predicate")
	}
}

func qrcodeClientForBody(body string) *Pan115Client {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	return New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
}

func TestFetchQRCodeImageValidatesHTTPResponse(t *testing.T) {
	if body, err := fetchQRCodeImage(nil, "https://example.invalid/qr"); body != nil || !errors.Is(err, ErrWrongParams) {
		t.Fatalf("fetchQRCodeImage nil client = %#v, %v; want nil, ErrWrongParams", body, err)
	}

	for name, tc := range map[string]struct {
		status int
		body   string
		want   error
	}{
		"not-found": {status: http.StatusNotFound, body: "missing", want: ErrUnexpected},
		"empty":     {status: http.StatusOK, body: "", want: ErrUnexpected},
		"success":   {status: http.StatusOK, body: "png-data", want: nil},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			body, err := fetchQRCodeImage(resty.New(), server.URL+"/qr")
			if tc.want != nil {
				if body != nil || !errors.Is(err, tc.want) {
					t.Fatalf("fetchQRCodeImage = %#v, %v; want nil, %v", body, err, tc.want)
				}
				return
			}
			if err != nil || string(body) != tc.body {
				t.Fatalf("fetchQRCodeImage = %q, %v", body, err)
			}
		})
	}
}

func TestFetchQRCodeImageSanitizesNetworkURL(t *testing.T) {
	transport := driverTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "https://qrcode.invalid/qr?uid=secret", Err: errors.New("network down")}
	})
	client := resty.NewWithClient(&http.Client{Transport: transport})
	body, err := fetchQRCodeImage(client, "https://qrcode.invalid/qr?uid=secret")
	if body != nil || err == nil {
		t.Fatalf("fetchQRCodeImage network failure = %#v, %v", body, err)
	}
	if strings.Contains(err.Error(), "uid=secret") || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("fetchQRCodeImage leaked query or lost cause: %v", err)
	}
}

func TestQRCodeStartRejectsIncompleteSuccessfulToken(t *testing.T) {
	client := qrcodeClientForBody(`{"state":1,"data":{}}`)
	session, err := client.QRCodeStart()
	if session != nil || !errors.Is(err, ErrUnexpected) {
		t.Fatalf("QRCodeStart incomplete success = %#v, %v; want nil, ErrUnexpected", session, err)
	}
}

func TestQRCodeLoginRejectsIncompleteSuccessfulCredential(t *testing.T) {
	client := qrcodeClientForBody(`{"state":1,"data":{"cookie":{}}}`)
	credential, err := client.QRCodeLoginWithApp(&QRCodeSession{UID: "uid"}, LoginAppTV)
	if credential != nil || !errors.Is(err, ErrUnexpected) {
		t.Fatalf("QRCodeLogin incomplete credential = %#v, %v; want nil, ErrUnexpected", credential, err)
	}
}

func TestQRCodeStatusRejectsMalformedSuccessfulResponse(t *testing.T) {
	session := &QRCodeSession{UID: "uid", Sign: "sign", Time: 1}
	for name, body := range map[string]string{
		"missing-data": `{"state":1}`,
		"null-data":    `{"state":1,"data":null}`,
		"unknown":      `{"state":1,"data":{"status":99}}`,
	} {
		t.Run(name, func(t *testing.T) {
			status, err := qrcodeClientForBody(body).QRCodeStatus(session)
			if status != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("QRCodeStatus malformed success = %#v, %v; want nil, ErrUnexpected", status, err)
			}
		})
	}
}

func TestQRCodeStatusAcceptsDocumentedStatuses(t *testing.T) {
	session := &QRCodeSession{UID: "uid", Sign: "sign", Time: 1}
	for _, want := range []int{-2, -1, 0, 1, 2} {
		body := fmt.Sprintf(`{"state":1,"data":{"status":%d}}`, want)
		status, err := qrcodeClientForBody(body).QRCodeStatus(session)
		if err != nil {
			t.Fatalf("QRCodeStatus(%d): %v", want, err)
		}
		if status == nil || status.Status != want {
			t.Fatalf("QRCodeStatus(%d) = %#v", want, status)
		}
	}
}

func TestQRCodeSessionRejectsIncompleteManualValues(t *testing.T) {
	session := &QRCodeSession{}
	if _, err := session.QRCode(); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("QRCode empty content error = %v, want ErrUnexpected", err)
	}
	if _, err := session.QRCodeByApi(); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeByApi empty uid error = %v, want ErrWrongParams", err)
	}
	client := New()
	if _, err := client.QRCodeLoginWithApp(session, LoginAppTV); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeLogin incomplete session error = %v, want ErrWrongParams", err)
	}
	if _, err := client.QRCodeStatus(session); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("QRCodeStatus incomplete session error = %v, want ErrWrongParams", err)
	}
}
