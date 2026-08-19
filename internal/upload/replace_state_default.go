//go:build !windows

package upload

import "os"

func replaceUploadStateFile(source, destination string) error {
	return os.Rename(source, destination)
}
