package driver

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
)

func TestUploadEntryPointsRejectInvalidInputsBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid upload input unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	valid := &UploadOSSParams{SHA1: "ABC", Bucket: "bucket", Object: "object"}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "oss-nil-params", call: func() error { return client.UploadByOSS(nil, strings.NewReader("x"), "0") }},
		{name: "oss-incomplete-params", call: func() error {
			return client.UploadByOSS(&UploadOSSParams{Bucket: "bucket"}, strings.NewReader("x"), "0")
		}},
		{name: "oss-nil-reader", call: func() error { return client.UploadByOSS(valid, nil, "0") }},
		{name: "multipart-nil-params", call: func() error { return client.UploadByMultipart(nil, 1, nil, "0") }},
		{name: "multipart-nil-file", call: func() error { return client.UploadByMultipart(valid, 1, nil, "0") }},
		{name: "multipart-negative-size", call: func() error { return client.UploadByMultipart(valid, -1, &os.File{}, "0") }},
		{name: "rapid-or-multipart-nil-file", call: func() error { return client.RapidUploadOrByMultipart("0", "file", 1, nil) }},
		{name: "rapid-or-multipart-negative-size", call: func() error { return client.RapidUploadOrByMultipart("0", "file", -1, &os.File{}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrWrongParams) {
				t.Fatalf("invalid upload input error = %v, want ErrWrongParams", err)
			}
		})
	}
}

func TestRapidUploadRejectsInvalidSourceBeforeNetwork(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid rapid upload source unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))
	if _, err := client.RapidUpload(1, "file", "0", "pre", "sha", nil); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("RapidUpload nil reader = %v, want ErrWrongParams", err)
	}
	if _, err := client.RapidUpload(-1, "file", "0", "pre", "sha", strings.NewReader("x")); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("RapidUpload negative size = %v, want ErrWrongParams", err)
	}
}

func TestRapidUploadRejectsHTTPFailureBeforeDecryptingBody(t *testing.T) {
	transport := driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     http.Header{"Content-Type": {"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("not encrypted response data")),
			Request:    req,
		}, nil
	})
	client := New(WithClient(&http.Client{Transport: transport}))
	client.UserID = 1
	client.Userkey = "key"
	client.UploadMetaInfo = &UploadMetaInfo{UploadAllowed: true, SizeLimit: 1024}

	result, err := client.RapidUpload(1, "file.bin", "0", "PRE", "FILESHA1", strings.NewReader("x"))
	if result != nil || !errors.Is(err, ErrUnexpected) || !strings.Contains(err.Error(), "503") {
		t.Fatalf("RapidUpload HTTP failure = %#v, %v; want nil, ErrUnexpected with status", result, err)
	}
	if strings.Contains(err.Error(), "not encrypted") {
		t.Fatalf("RapidUpload attempted to interpret HTTP error body: %v", err)
	}
}

func TestRapidUploadConvenienceWrappersRejectDeclaredSizeMismatch(t *testing.T) {
	client := New()
	client.UserID = 1
	client.Userkey = "key"
	client.UploadMetaInfo = &UploadMetaInfo{UploadAllowed: true, SizeLimit: 1024}

	if err := client.RapidUploadOrByOSS("0", "file", 2, strings.NewReader("abc")); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("RapidUploadOrByOSS size mismatch = %v, want ErrWrongParams", err)
	}

	file, err := os.CreateTemp(t.TempDir(), "upload-size-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := client.RapidUploadOrByMultipart("0", "file", 2, file); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("RapidUploadOrByMultipart size mismatch = %v, want ErrWrongParams", err)
	}
}

func TestUploadDigestRangeValidatesAndHashesRequestedBytes(t *testing.T) {
	client := New()
	if _, err := client.UploadDigestRange(nil, "0-1"); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("UploadDigestRange nil reader = %v, want ErrWrongParams", err)
	}
	for _, rangeSpec := range []string{"", "1", "3-1", "-1-2", "a-b"} {
		if _, err := client.UploadDigestRange(strings.NewReader("abcdef"), rangeSpec); !errors.Is(err, ErrWrongParams) {
			t.Fatalf("UploadDigestRange(%q) = %v, want ErrWrongParams", rangeSpec, err)
		}
	}
	got, err := client.UploadDigestRange(strings.NewReader("abcdef"), "1-3")
	if err != nil {
		t.Fatal(err)
	}
	wantSum := sha1.Sum([]byte("bcd"))
	want := strings.ToUpper(hex.EncodeToString(wantSum[:]))
	if got != want {
		t.Fatalf("UploadDigestRange = %q, want %q", got, want)
	}
}

func TestRapidUploadSignChallengeIsBoundedAndValidated(t *testing.T) {
	client := New()
	if _, _, retry, err := client.resolveRapidUploadSignChallenge(&UploadInitResp{Status: 1}, 0, strings.NewReader("abc")); err != nil || retry {
		t.Fatalf("non-challenge response = retry %v, err %v", retry, err)
	}
	for name, result := range map[string]*UploadInitResp{
		"nil":           nil,
		"missing-key":   {Status: 7, SignCheck: "0-1"},
		"missing-range": {Status: 7, SignKey: "key"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := client.resolveRapidUploadSignChallenge(result, 0, strings.NewReader("abc")); !errors.Is(err, ErrUnexpected) {
				t.Fatalf("challenge validation = %v, want ErrUnexpected", err)
			}
		})
	}
	if _, _, _, err := client.resolveRapidUploadSignChallenge(&UploadInitResp{Status: 7, SignKey: "key", SignCheck: "0-1"}, maxRapidUploadSignChallenges, strings.NewReader("abc")); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("challenge limit = %v, want ErrUnexpected", err)
	}
	signKey, signVal, retry, err := client.resolveRapidUploadSignChallenge(&UploadInitResp{Status: 7, SignKey: " key ", SignCheck: "1-2"}, 0, strings.NewReader("abc"))
	if err != nil || !retry || signKey != "key" || signVal == "" {
		t.Fatalf("valid challenge = key %q val %q retry %v err %v", signKey, signVal, retry, err)
	}
}

func TestUploadByMultipartRejectsDeclaredFileSizeMismatchBeforeNetwork(t *testing.T) {
	client := New(WithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("multipart size mismatch unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})}))
	file, err := os.CreateTemp(t.TempDir(), "multipart-size-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("abc"); err != nil {
		t.Fatal(err)
	}
	params := &UploadOSSParams{SHA1: "ABC", Bucket: "bucket", Object: "object"}
	if err := client.UploadByMultipart(params, 2, file, "0"); !errors.Is(err, ErrWrongParams) {
		t.Fatalf("UploadByMultipart size mismatch = %v, want ErrWrongParams", err)
	}
}

func TestSplitFilePartNumHandlesExactNineGiBBoundary(t *testing.T) {
	for size, want := range map[int64]int{
		0:        1000,
		GB - 1:   1000,
		GB:       2000,
		9*GB - 1: 9000,
		9 * GB:   10000,
		10 * GB:  10000,
	} {
		if got := splitFilePartNum(size); got != want {
			t.Fatalf("splitFilePartNum(%d) = %d, want %d", size, got, want)
		}
	}
}

func TestUploadByMultipartRejectsInvalidTimingOptionsBeforeNetwork(t *testing.T) {
	client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: driverTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid multipart timing unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	params := &UploadOSSParams{SHA1: "ABC", Bucket: "bucket", Object: "object"}
	file, err := os.CreateTemp(t.TempDir(), "upload-input-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	for name, option := range map[string]UploadMultipartOption{
		"zero-timeout":       UploadMultipartWithTimeout(0),
		"negative-timeout":   UploadMultipartWithTimeout(-time.Second),
		"zero-token-refresh": UploadMultipartWithTokenRefreshTime(0),
		"negative-refresh":   UploadMultipartWithTokenRefreshTime(-time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			if err := client.UploadByMultipart(params, 0, file, "0", nil, option); !errors.Is(err, ErrWrongParams) {
				t.Fatalf("invalid multipart timing error = %v, want ErrWrongParams", err)
			}
		})
	}
}

func TestOssOptionNilInputsAreSafe(t *testing.T) {
	if got := OssOption(nil, nil); len(got) != 0 {
		t.Fatalf("OssOption(nil, nil) = %#v, want no options", got)
	}
}
