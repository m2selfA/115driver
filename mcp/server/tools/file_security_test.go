package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateUploadURLRejectsUnsafeTargets(t *testing.T) {
	tests := []string{
		"file:///etc/passwd",
		"ftp://example.com/file.bin",
		"http://127.0.0.1/file.bin",
		"http://localhost/file.bin",
		"http://[::1]/file.bin",
	}

	for _, rawURL := range tests {
		t.Run(rawURL, func(t *testing.T) {
			if _, err := validateUploadURL(rawURL); err == nil {
				t.Fatalf("expected %q to be rejected", rawURL)
			}
		})
	}
}

func TestValidateUploadURLMalformedErrorDoesNotEchoSource(t *testing.T) {
	const rawURL = "https://user:password@example.com/%zz?token=super-secret#fragment"
	_, err := validateUploadURL(rawURL)
	if err == nil {
		t.Fatal("malformed URL was accepted")
	}
	for _, secret := range []string{"user", "password", "%zz", "token", "super-secret", "fragment"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("malformed URL error leaked %q: %v", secret, err)
		}
	}
	if err.Error() != "malformed URL" {
		t.Fatalf("malformed URL error = %q", err.Error())
	}
}

func TestSanitizeMCPExternalURLErrorRedactsURLSecretsRecursively(t *testing.T) {
	inner := &url.Error{
		Op:  "dial",
		URL: "https://inner-user:inner-pass@inner.example/private?inner_token=secret#inner-fragment",
		Err: errors.New("network down"),
	}
	outer := &url.Error{
		Op:  "Get",
		URL: "https://outer-user:outer-pass@outer.example/private/file?download_token=secret#fragment",
		Err: inner,
	}
	safe := sanitizeMCPExternalURLError(outer)
	if safe == nil {
		t.Fatal("sanitized URL error is nil")
	}
	text := safe.Error()
	for _, secret := range []string{"outer-user", "outer-pass", "/private", "download_token", "inner-user", "inner-pass", "inner_token", "fragment"} {
		if strings.Contains(text, secret) {
			t.Fatalf("sanitized URL error leaked %q: %s", secret, text)
		}
	}
	for _, diagnostic := range []string{"outer.example", "inner.example", "network down"} {
		if !strings.Contains(text, diagnostic) {
			t.Fatalf("sanitized URL error lost %q: %s", diagnostic, text)
		}
	}
}

func TestValidateUploadURLAcceptsHTTPSURL(t *testing.T) {
	got, err := validateUploadURL("https://example.com/path/file.bin?token=abc")
	if err != nil {
		t.Fatalf("expected URL to be accepted: %v", err)
	}
	if got.String() != "https://example.com/path/file.bin?token=abc" {
		t.Fatalf("unexpected parsed URL: %s", got.String())
	}
}

func TestNormalizeLocalRootValidatesAndCanonicalizesConfiguredBoundary(t *testing.T) {
	if got, err := NormalizeLocalRoot("   "); err != nil || got != "" {
		t.Fatalf("empty local root = %q, %v; want disabled", got, err)
	}

	root := t.TempDir()
	got, err := NormalizeLocalRoot(root)
	if err != nil {
		t.Fatalf("normalize local root: %v", err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	want, err = filepath.Abs(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Clean(want) {
		t.Fatalf("normalized local root = %q, want %q", got, filepath.Clean(want))
	}

	if _, err := NormalizeLocalRoot(filepath.Join(root, "missing")); err == nil {
		t.Fatal("missing local root was accepted")
	}
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NormalizeLocalRoot(file); err == nil {
		t.Fatal("regular file was accepted as local root")
	}
}

func TestValidateLocalPathRequiresConfiguredRoot(t *testing.T) {
	if _, err := validateLocalPath("", "/tmp/out.bin", false); err == nil {
		t.Fatal("expected empty local root to reject local file access")
	}
}

func TestValidateLocalPathRejectsPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	if _, err := validateLocalPath(root, root+"/../outside.bin", false); err == nil {
		t.Fatal("expected path outside root to be rejected")
	}
}

func TestValidateLocalPathAcceptsPathInsideRoot(t *testing.T) {
	root := t.TempDir()
	got, err := validateLocalPath(root, root+"/nested/out.bin", false)
	if err != nil {
		t.Fatalf("expected path inside root to be accepted: %v", err)
	}
	if !strings.HasPrefix(got, root) {
		t.Fatalf("expected %q to stay under %q", got, root)
	}
}

func TestValidateLocalPathRejectsExistingSymlinkFileOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if _, err := validateLocalPath(root, link, false); err == nil {
		t.Fatal("expected symlink target outside root to be rejected")
	}
}

func TestValidateLocalPathRejectsMissingPathUnderSymlinkedParentOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(link, "missing", "file.txt")

	if _, err := validateLocalPath(root, target, false); err == nil {
		t.Fatal("expected missing path below external symlink parent to be rejected")
	}
}

func TestCopyHTTPResponseRequiresStatusOK(t *testing.T) {
	var out strings.Builder
	resp := &http.Response{
		StatusCode: http.StatusForbidden,
		Body:       io.NopCloser(strings.NewReader("denied")),
	}

	err := copyHTTPResponse(&out, resp, 1024)
	if err == nil {
		t.Fatal("expected non-200 response to fail")
	}
	if !errors.Is(err, errUnexpectedHTTPStatus) {
		t.Fatalf("expected errUnexpectedHTTPStatus, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected response body not to be copied, wrote %d bytes", out.Len())
	}
}

func TestCopyHTTPResponseEnforcesSizeLimit(t *testing.T) {
	var out strings.Builder
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("abcdef")),
	}

	err := copyHTTPResponse(&out, resp, 3)
	if err == nil {
		t.Fatal("expected oversized response to fail")
	}
	if !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("expected errResponseTooLarge, got %v", err)
	}
}

func TestCopyHTTPResponseAllowsUnlimitedSizeWhenLimitIsZero(t *testing.T) {
	var out strings.Builder
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("abcdef")),
	}

	if err := copyHTTPResponse(&out, resp, 0); err != nil {
		t.Fatalf("expected zero limit to allow response: %v", err)
	}
	if out.String() != "abcdef" {
		t.Fatalf("unexpected copied body: %q", out.String())
	}
}

func TestCopyHTTPResponseRejectsNegativeSizeLimit(t *testing.T) {
	var out strings.Builder
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("abcdef")),
	}

	err := copyHTTPResponse(&out, resp, -1)
	if err == nil {
		t.Fatal("expected negative size limit to fail")
	}
	if !errors.Is(err, errInvalidSizeLimit) {
		t.Fatalf("expected errInvalidSizeLimit, got %v", err)
	}
}

func TestMCPDefaultDownloadSizeAllowsLargeDownloads(t *testing.T) {
	if defaultMCPDownloadMaxBytes != 0 {
		t.Fatalf("expected default MCP download size to be unlimited, got %d", defaultMCPDownloadMaxBytes)
	}
}

func TestMCPDefaultURLUploadSizeRemainsBounded(t *testing.T) {
	if defaultMCPURLUploadMaxBytes <= 0 {
		t.Fatalf("expected URL upload default size to be bounded, got %d", defaultMCPURLUploadMaxBytes)
	}
}

func TestMCPURLUploadHTTPClientUsesConfiguredTimeout(t *testing.T) {
	client := newMCPURLUploadHTTPClient(90 * time.Second)
	if client.Timeout != 90*time.Second {
		t.Fatalf("expected configured timeout, got %s", client.Timeout)
	}
}

func TestMCPURLUploadHTTPClientRejectsUnsafeRedirect(t *testing.T) {
	client := newMCPURLUploadHTTPClient(90 * time.Second)
	req := &http.Request{URL: mustParseURL(t, "http://127.0.0.1/private")}
	if err := client.CheckRedirect(req, nil); err == nil {
		t.Fatal("expected redirect to unsafe host to be rejected")
	}
}

func TestValidateResolvedIPsRejectsPrivateAddress(t *testing.T) {
	if err := validateResolvedIPs("example.com", []net.IP{net.ParseIP("10.0.0.1")}); err == nil {
		t.Fatal("expected private resolved IP to be rejected")
	}
}

func TestDialResolvedIPsFallsBackToLaterAddresses(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("203.0.113.1"),
		net.ParseIP("203.0.113.2"),
	}
	var attempted []string
	conn, err := dialResolvedIPs(context.Background(), "tcp", "example.com", "443", ips, func(ctx context.Context, network, address string) (net.Conn, error) {
		attempted = append(attempted, address)
		if len(attempted) == 1 {
			return nil, errors.New("first address unreachable")
		}
		client, server := net.Pipe()
		server.Close()
		return client, nil
	})
	if err != nil {
		t.Fatalf("expected second address to be used: %v", err)
	}
	conn.Close()

	want := []string{
		net.JoinHostPort("203.0.113.1", "443"),
		net.JoinHostPort("203.0.113.2", "443"),
	}
	if strings.Join(attempted, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected dial attempts: got %v want %v", attempted, want)
	}
}

func TestMCPDefaultDownloadTimeoutAllowsLargeDownloads(t *testing.T) {
	if defaultMCPDownloadTimeout < time.Hour {
		t.Fatalf("expected default timeout to allow large downloads, got %s", defaultMCPDownloadTimeout)
	}
}

func mustParseURL(t *testing.T, rawURL string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
