package transfer

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestNetworkPathsFromInterfacesFiltersAndSortsCandidates(t *testing.T) {
	interfaces := []net.Interface{
		{Index: 9, Name: "down", Flags: 0},
		{Index: 7, Name: "ethernet-b", Flags: net.FlagUp},
		{Index: 3, Name: "ethernet-a", Flags: net.FlagUp},
		{Index: 1, Name: "loopback", Flags: net.FlagUp | net.FlagLoopback},
	}
	addresses := map[int][]net.Addr{
		7: {
			&net.IPNet{IP: net.ParseIP("2001:db8::2"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("10.0.0.7"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("169.254.1.7"), Mask: net.CIDRMask(16, 32)},
		},
		3: {
			&net.IPNet{IP: net.ParseIP("192.168.10.3"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.10.3"), Mask: net.CIDRMask(24, 32)},
		},
		1: {
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		},
	}

	paths, err := networkPathsFromInterfaces(interfaces, func(iface net.Interface) ([]net.Addr, error) {
		return addresses[iface.Index], nil
	})
	if err != nil {
		t.Fatalf("unexpected enumeration error: %v", err)
	}

	if len(paths) != 3 {
		t.Fatalf("expected 3 usable unique paths, got %d: %#v", len(paths), paths)
	}
	if paths[0].InterfaceIndex != 3 || paths[0].LocalIP.String() != "192.168.10.3" {
		t.Fatalf("unexpected first path: %#v", paths[0])
	}
	if paths[1].InterfaceIndex != 7 || paths[1].LocalIP.String() != "10.0.0.7" {
		t.Fatalf("unexpected second path: %#v", paths[1])
	}
	if paths[2].InterfaceIndex != 7 || paths[2].LocalIP.String() != "2001:db8::2" {
		t.Fatalf("unexpected third path: %#v", paths[2])
	}
}

func TestNetworkPathsFromInterfacesReturnsPartialResultsWithErrors(t *testing.T) {
	interfaces := []net.Interface{
		{Index: 2, Name: "good", Flags: net.FlagUp},
		{Index: 4, Name: "broken", Flags: net.FlagUp},
	}

	paths, err := networkPathsFromInterfaces(interfaces, func(iface net.Interface) ([]net.Addr, error) {
		if iface.Name == "broken" {
			return nil, errors.New("address lookup failed")
		}
		return []net.Addr{&net.IPAddr{IP: net.ParseIP("10.0.0.2")}}, nil
	})
	if len(paths) != 1 {
		t.Fatalf("expected partial result, got %#v", paths)
	}
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("expected interface-specific error, got %v", err)
	}
}

func TestNetworkPathValidate(t *testing.T) {
	valid := NetworkPath{InterfaceName: "ethernet", InterfaceIndex: 7, LocalIP: net.ParseIP("10.0.0.7")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("expected valid path: %v", err)
	}
	if valid.Network() != "tcp4" {
		t.Fatalf("expected tcp4, got %q", valid.Network())
	}

	tests := []NetworkPath{
		{InterfaceName: "missing-index", LocalIP: net.ParseIP("10.0.0.7")},
		{InterfaceName: "missing-ip", InterfaceIndex: 7},
		{InterfaceName: "loopback", InterfaceIndex: 7, LocalIP: net.ParseIP("127.0.0.1")},
		{InterfaceName: "link-local", InterfaceIndex: 7, LocalIP: net.ParseIP("169.254.1.1")},
	}
	for _, path := range tests {
		if err := path.Validate(); err == nil {
			t.Fatalf("expected invalid path to fail validation: %#v", path)
		}
	}
}

func TestNewDialerBindsLocalAddress(t *testing.T) {
	path := NetworkPath{InterfaceName: "ethernet", InterfaceIndex: 7, LocalIP: net.ParseIP("10.0.0.7")}
	dialer, err := NewDialer(path)
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr, ok := dialer.LocalAddr.(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP local address, got %T", dialer.LocalAddr)
	}
	if !tcpAddr.IP.Equal(net.ParseIP("10.0.0.7")) {
		t.Fatalf("unexpected bound local IP: %s", tcpAddr.IP)
	}
}

func TestNewTransportUsesBoundDialer(t *testing.T) {
	path := NetworkPath{InterfaceName: "ethernet", InterfaceIndex: 7, LocalIP: net.ParseIP("10.0.0.7")}
	transport, err := NewTransport(path)
	if err != nil {
		t.Fatal(err)
	}
	if transport.DialContext == nil {
		t.Fatal("expected custom DialContext")
	}
	if transport == httpDefaultTransportForTest() {
		t.Fatal("expected default transport to be cloned, not mutated")
	}
}

func httpDefaultTransportForTest() *http.Transport {
	return http.DefaultTransport.(*http.Transport)
}
