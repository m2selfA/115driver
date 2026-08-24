//go:build windows

package transfer

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

const windowsReplaceRetryAttempts = 8

func replaceDownloadedFile(from, to string) error {
	fromPtr, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}

	delay := 5 * time.Millisecond
	for attempt := 0; attempt < windowsReplaceRetryAttempts; attempt++ {
		err = windows.MoveFileEx(
			fromPtr,
			toPtr,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil {
			return nil
		}
		if !isRetryableWindowsReplaceError(err) || attempt+1 == windowsReplaceRetryAttempts {
			return err
		}
		time.Sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
			if delay > 100*time.Millisecond {
				delay = 100 * time.Millisecond
			}
		}
	}
	return err
}

func isRetryableWindowsReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
