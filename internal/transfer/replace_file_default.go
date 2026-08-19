//go:build !windows

package transfer

import "os"

func replaceDownloadedFile(from, to string) error {
	return os.Rename(from, to)
}
