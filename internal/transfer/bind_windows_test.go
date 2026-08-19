//go:build windows

package transfer

import "testing"

func TestWindowsIPv4InterfaceIndexValueUsesNetworkByteOrder(t *testing.T) {
	const interfaceIndex = 7
	got := uint32(windowsIPv4InterfaceIndexValue(interfaceIndex))
	const want uint32 = 0x07000000
	if got != want {
		t.Fatalf("unexpected socket option value: got %#x want %#x", got, want)
	}
}
