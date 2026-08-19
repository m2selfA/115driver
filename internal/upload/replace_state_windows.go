//go:build windows

package upload

import "golang.org/x/sys/windows"

func replaceUploadStateFile(source, destination string) error {
	fromPtr, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(fromPtr, toPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
