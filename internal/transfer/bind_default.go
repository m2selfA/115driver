//go:build !windows

package transfer

// Non-Windows platforms use net.Dialer.LocalAddr as the portable binding
// mechanism. Platform-specific stronger binding can be added here later if
// needed without changing callers.
func bindInterfaceControl(NetworkPath) dialControlFunc {
	return nil
}
