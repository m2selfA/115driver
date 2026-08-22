package driver

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/go-resty/resty/v2"
)

// recordingTransport records the User-Agent of each request as seen at the
// transport layer (i.e. what net/http would write to the wire) and can
// rewrite the request URL to a mock server, which is needed because the
// 115 download API endpoints are absolute URLs.
type recordingTransport struct {
	base    http.RoundTripper
	mockURL *url.URL
	wireUAs []string
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.wireUAs = append(t.wireUAs, req.Header.Get("User-Agent"))
	if t.mockURL != nil {
		req.URL.Scheme = t.mockURL.Scheme
		req.URL.Host = t.mockURL.Host
	}
	return t.base.RoundTrip(req)
}

// newUATestEnv starts a mock server that records the received User-Agent and
// returns it together with a recording transport and a client wired to it.
func newUATestEnv(t *testing.T) (*httptest.Server, *recordingTransport, *[]string) {
	t.Helper()
	var serverUAs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverUAs = append(serverUAs, r.Header.Get("User-Agent"))
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"state":false,"error":"mock API"}`))
	}))
	tr := &recordingTransport{base: &http.Transport{}}
	return server, tr, &serverUAs
}

func assertNoUA(t *testing.T, wireUAs, serverUAs []string) {
	t.Helper()
	if len(wireUAs) == 0 {
		t.Fatal("no request observed at transport layer")
	}
	for i, ua := range wireUAs {
		if ua != "" {
			t.Fatalf("transport UA #%d = %q, want empty", i, ua)
		}
	}
	for i, ua := range serverUAs {
		if ua != "" {
			t.Fatalf("server UA #%d = %q, want empty", i, ua)
		}
	}
}

// TestEmptyUAHandling verifies that an explicitly empty User-Agent never
// reaches the wire as resty's default UA, and that the headers visible after
// the response reflect what was actually sent.
func TestEmptyUAHandling(t *testing.T) {
	t.Run("request-level-empty-string", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New(WithClient(&http.Client{Transport: tr}))

		resp, err := client.NewRequest().SetHeader("User-Agent", "").Post(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
		if got := resp.Request.Header.Get("User-Agent"); got != "" {
			t.Fatalf("resp.Request.Header UA = %q, want empty (OnAfterResponse cleanup)", got)
		}
		if got := sentRequestHeaders(resp).Get("User-Agent"); got != "" {
			t.Fatalf("sentRequestHeaders UA = %q, want empty", got)
		}
	})

	t.Run("request-level-whitespace", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New(WithClient(&http.Client{Transport: tr}))

		if _, err := client.NewRequest().SetHeader("User-Agent", " ").Post(server.URL); err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
	})

	t.Run("request-level-nil-values", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New(WithClient(&http.Client{Transport: tr}))

		r := client.NewRequest()
		r.Header["User-Agent"] = nil
		if _, err := r.Post(server.URL); err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
	})

	t.Run("real-ua-preserved", func(t *testing.T) {
		server, tr, _ := newUATestEnv(t)
		defer server.Close()
		client := New(WithClient(&http.Client{Transport: tr}))
		const ua = "Mozilla/5.0 115Browser/26.0.0.3"

		if _, err := client.NewRequest().SetHeader("User-Agent", ua).Post(server.URL); err != nil {
			t.Fatal(err)
		}
		if len(tr.wireUAs) != 1 || tr.wireUAs[0] != ua {
			t.Fatalf("wire UA = %v, want %q", tr.wireUAs, ua)
		}
	})

	t.Run("client-level-empty", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New(WithClient(&http.Client{Transport: tr}))
		client.SetUserAgent("")

		if _, err := client.NewRequest().Post(server.URL); err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
	})

	t.Run("after-set-http-client", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New()
		client.SetHttpClient(&http.Client{Transport: tr})

		if _, err := client.NewRequest().SetHeader("User-Agent", "").Post(server.URL); err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
	})

	t.Run("after-with-resty-client", func(t *testing.T) {
		server, tr, serverUAs := newUATestEnv(t)
		defer server.Close()
		client := New(WithRestyClient(resty.NewWithClient(&http.Client{Transport: tr})))

		if _, err := client.NewRequest().SetHeader("User-Agent", "").Post(server.URL); err != nil {
			t.Fatal(err)
		}
		assertNoUA(t, tr.wireUAs, *serverUAs)
	})
}

func TestConcurrentRequestsKeepUserAgentStateRequestLocal(t *testing.T) {
	const requestCount = 32
	type observation struct {
		id string
		ua string
	}
	var (
		mu           sync.Mutex
		observations = make([]observation, 0, requestCount)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		observations = append(observations, observation{id: r.Header.Get("X-Request-ID"), ua: r.Header.Get("User-Agent")})
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := New()
	client.SetUserAgent("")
	var wg sync.WaitGroup
	errs := make(chan error, requestCount)
	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			id := fmt.Sprintf("request-%02d", index)
			req := client.NewRequest().SetHeader("X-Request-ID", id)
			wantUA := ""
			if index%2 == 1 {
				wantUA = fmt.Sprintf("sync-test/%d", index)
				req.SetHeader("User-Agent", wantUA)
			}
			resp, err := req.Post(server.URL)
			if err != nil {
				errs <- fmt.Errorf("%s: %w", id, err)
				return
			}
			if got := sentRequestHeaders(resp).Get("User-Agent"); got != wantUA {
				errs <- fmt.Errorf("%s sent UA %q, want %q", id, got, wantUA)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	mu.Lock()
	gotObservations := append([]observation(nil), observations...)
	mu.Unlock()
	if len(gotObservations) != requestCount {
		t.Fatalf("server observed %d request(s), want %d", len(gotObservations), requestCount)
	}
	seen := make(map[string]string, requestCount)
	for _, got := range gotObservations {
		if _, duplicate := seen[got.id]; duplicate {
			t.Fatalf("duplicate request id observed: %q", got.id)
		}
		seen[got.id] = got.ua
	}
	for i := 0; i < requestCount; i++ {
		id := fmt.Sprintf("request-%02d", i)
		wantUA := ""
		if i%2 == 1 {
			wantUA = fmt.Sprintf("sync-test/%d", i)
		}
		if got, ok := seen[id]; !ok || got != wantUA {
			t.Fatalf("server request %q UA=%q present=%v, want %q", id, got, ok, wantUA)
		}
	}
	if got := client.Client.Header.Get("User-Agent"); got != "" {
		t.Fatalf("client-level User-Agent mutated by concurrent requests: %q", got)
	}
}

func TestNewRequestKeepsLegacyLastRequestWithoutReusingRequestObjects(t *testing.T) {
	client := New()
	first := client.NewRequest()
	second := client.NewRequest()
	if first == second {
		t.Fatal("NewRequest reused a request object")
	}
	if got := client.GetRequest(); got != second {
		t.Fatalf("GetRequest returned %p, want most recent request %p", got, second)
	}
}

func TestSafeDebugLoggingDoesNotExposeCredentials(t *testing.T) {
	const (
		querySecret         = "query-secret-value"
		cookieSecret        = "cookie-secret-value"
		authorizationSecret = "authorization-secret-value"
		ossTokenSecret      = "oss-token-secret-value"
		requestBodySecret   = "request-body-secret-value"
		responseBodySecret  = "response-body-secret-value"
		responseCookie      = "response-cookie-secret-value"
	)

	var gotQuery, gotCookie, gotAuthorization, gotOSSToken, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("k_ec")
		gotCookie = r.Header.Get("Cookie")
		gotAuthorization = r.Header.Get("Authorization")
		gotOSSToken = r.Header.Get("X-Oss-Security-Token")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Set-Cookie", "session="+responseCookie)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"AccessKeySecret":"` + responseBodySecret + `","SecurityToken":"` + ossTokenSecret + `"}`))
	}))
	defer server.Close()

	var logs bytes.Buffer
	client := New()
	client.debugWriter = &logs
	client.SetDebug(true)
	resp, err := client.NewRequest().
		SetDebug(true).
		SetHeader("Cookie", "session="+cookieSecret).
		SetHeader("Authorization", "Bearer "+authorizationSecret).
		SetHeader("X-Oss-Security-Token", ossTokenSecret).
		SetBody(`{"password":"` + requestBodySecret + `"}`).
		Post(server.URL + "/upload?k_ec=" + url.QueryEscape(querySecret) + "&plain=visible")
	if err != nil {
		t.Fatal(err)
	}

	if gotQuery != querySecret || gotCookie != "session="+cookieSecret || gotAuthorization != "Bearer "+authorizationSecret || gotOSSToken != ossTokenSecret || !strings.Contains(gotBody, requestBodySecret) {
		t.Fatalf("debug redaction changed wire request: query=%q cookie=%q auth=%q token=%q body=%q", gotQuery, gotCookie, gotAuthorization, gotOSSToken, gotBody)
	}
	if client.Client.Debug {
		t.Fatal("raw Resty client debug must remain disabled")
	}
	if resp.Request.Debug {
		t.Fatal("request-level Resty debug must be disabled before logging")
	}

	logText := logs.String()
	for _, secret := range []string{querySecret, cookieSecret, authorizationSecret, ossTokenSecret, requestBodySecret, responseBodySecret, responseCookie} {
		if strings.Contains(logText, secret) {
			t.Fatalf("debug log leaked secret %q:\n%s", secret, logText)
		}
	}
	for _, want := range []string{
		"115driver DEBUG request method=POST",
		"/upload?k_ec=%5BREDACTED%5D&plain=%5BREDACTED%5D",
		"115driver DEBUG response status=200",
	} {
		if !strings.Contains(logText, want) {
			t.Fatalf("debug log missing %q:\n%s", want, logText)
		}
	}
}

// TestDownloadWithUA_EmptyUA_SendsNoUA exercises the real DownloadWithUA call
// (with the actual absolute API URL) against a mock server through the
// recording transport. The mock returns an API error so the request follows
// the error path — the 115 response encryption cannot be mocked without the
// server's RSA private exponent, so a successful decrypted response is not
// feasible in tests.
func TestDownloadWithUA_EmptyUA_SendsNoUA(t *testing.T) {
	server, tr, serverUAs := newUATestEnv(t)
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	tr.mockURL = u
	client := New(WithClient(&http.Client{Transport: tr}))

	info, err := client.DownloadWithUA("pickcode", "")
	if err == nil {
		t.Fatalf("expected mock API error, got info=%v", info)
	}
	assertNoUA(t, tr.wireUAs, *serverUAs)
}
