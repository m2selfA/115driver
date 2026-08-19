package transfer

import (
	"net"
	"net/http"
	"syscall"
	"time"
)

const (
	defaultDialTimeout = 30 * time.Second
	defaultKeepAlive   = 30 * time.Second
)

type dialControlFunc func(network, address string, conn syscall.RawConn) error

type transportFactory func(NetworkPath) (http.RoundTripper, error)

// NewDialer creates a TCP dialer pinned to path. LocalAddr provides the
// cross-platform source-address binding; platform-specific control hooks may
// add a stronger interface constraint where the OS supports it.
func NewDialer(path NetworkPath) (*net.Dialer, error) {
	if err := path.Validate(); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: defaultKeepAlive,
		LocalAddr: &net.TCPAddr{IP: canonicalIP(path.LocalIP)},
	}
	dialer.Control = bindInterfaceControl(path)
	return dialer, nil
}

// NewTransport creates an HTTP transport whose new connections are pinned to
// path. It clones http.DefaultTransport so normal proxy, TLS, keep-alive and
// HTTP/2 defaults remain intact while only the dial path is replaced.
func NewTransport(path NetworkPath) (*http.Transport, error) {
	dialer, err := NewDialer(path)
	if err != nil {
		return nil, err
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext
	return transport, nil
}
