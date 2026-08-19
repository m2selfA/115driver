package transfer

import (
	"errors"
	"fmt"
	"net"
	"sort"
)

// NetworkPath identifies one usable local address on one network interface.
// A physical interface can produce multiple paths when it has multiple usable
// IPv4 or IPv6 addresses.
type NetworkPath struct {
	InterfaceName  string
	InterfaceIndex int
	LocalIP        net.IP
}

// String returns a compact human-readable representation of the network path.
func (p NetworkPath) String() string {
	return fmt.Sprintf("%s[%d]@%s", p.InterfaceName, p.InterfaceIndex, p.LocalIP)
}

// Network returns the TCP network matching the path's local address family.
func (p NetworkPath) Network() string {
	if p.LocalIP.To4() != nil {
		return "tcp4"
	}
	return "tcp6"
}

// Validate checks whether the path can be used as an outbound transfer path.
func (p NetworkPath) Validate() error {
	if p.InterfaceIndex <= 0 {
		return fmt.Errorf("invalid interface index %d", p.InterfaceIndex)
	}
	ip := canonicalIP(p.LocalIP)
	if ip == nil {
		return errors.New("network path has no valid local IP")
	}
	if !ip.IsGlobalUnicast() {
		return fmt.Errorf("local IP %s is not a usable unicast address", ip)
	}
	return nil
}

// ListNetworkPaths enumerates active non-loopback interfaces and returns one
// path for every usable unicast IPv4/IPv6 address. If address enumeration fails
// for an individual interface, paths from other interfaces are still returned
// together with the joined enumeration error.
func ListNetworkPaths() ([]NetworkPath, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network interfaces: %w", err)
	}
	return networkPathsFromInterfaces(interfaces, func(iface net.Interface) ([]net.Addr, error) {
		return iface.Addrs()
	})
}

type interfaceAddrsFunc func(net.Interface) ([]net.Addr, error)

func networkPathsFromInterfaces(interfaces []net.Interface, addrs interfaceAddrsFunc) ([]NetworkPath, error) {
	paths := make([]NetworkPath, 0)
	seen := make(map[string]struct{})
	var errs []error

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		addresses, err := addrs(iface)
		if err != nil {
			errs = append(errs, fmt.Errorf("list addresses for interface %q: %w", iface.Name, err))
			continue
		}
		for _, addr := range addresses {
			ip := ipFromAddr(addr)
			if ip == nil || !ip.IsGlobalUnicast() {
				continue
			}

			key := fmt.Sprintf("%d|%s", iface.Index, ip.String())
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			paths = append(paths, NetworkPath{
				InterfaceName:  iface.Name,
				InterfaceIndex: iface.Index,
				LocalIP:        ip,
			})
		}
	}

	sort.Slice(paths, func(i, j int) bool {
		if paths[i].InterfaceIndex != paths[j].InterfaceIndex {
			return paths[i].InterfaceIndex < paths[j].InterfaceIndex
		}
		return paths[i].LocalIP.String() < paths[j].LocalIP.String()
	})
	return paths, errors.Join(errs...)
}

func ipFromAddr(addr net.Addr) net.IP {
	switch value := addr.(type) {
	case *net.IPNet:
		return canonicalIP(value.IP)
	case *net.IPAddr:
		return canonicalIP(value.IP)
	default:
		return nil
	}
}

func canonicalIP(ip net.IP) net.IP {
	if ip == nil {
		return nil
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return append(net.IP(nil), ipv4...)
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return append(net.IP(nil), ipv6...)
	}
	return nil
}
