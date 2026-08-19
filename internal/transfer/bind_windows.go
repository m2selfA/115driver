//go:build windows

package transfer

import (
	"math/bits"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows SDK ws2ipdef.h defines both unicast interface options as 31. The
// IPv4 option requires the interface index in network byte order, whereas the
// IPv6 option takes the interface index in host byte order.
const (
	windowsIPUnicastIF   = 31
	windowsIPv6UnicastIF = 31
)

func bindInterfaceControl(path NetworkPath) dialControlFunc {
	interfaceIndex := path.InterfaceIndex
	ipv4 := path.LocalIP.To4() != nil

	return func(network, address string, conn syscall.RawConn) error {
		var socketErr error
		controlErr := conn.Control(func(fd uintptr) {
			if ipv4 {
				socketErr = windows.SetsockoptInt(
					windows.Handle(fd),
					windows.IPPROTO_IP,
					windowsIPUnicastIF,
					windowsIPv4InterfaceIndexValue(interfaceIndex),
				)
				return
			}
			socketErr = windows.SetsockoptInt(
				windows.Handle(fd),
				windows.IPPROTO_IPV6,
				windowsIPv6UnicastIF,
				interfaceIndex,
			)
		})
		if controlErr != nil {
			return controlErr
		}
		return socketErr
	}
}

func windowsIPv4InterfaceIndexValue(interfaceIndex int) int {
	return int(bits.ReverseBytes32(uint32(interfaceIndex)))
}
