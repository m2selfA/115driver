//go:build windows

package transfer

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

const (
	windowsReplaceRetryWindow   = 5 * time.Second
	windowsReplaceMaxRetryDelay = 250 * time.Millisecond
)

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
	deadline := time.Now().Add(windowsReplaceRetryWindow)
	for {
		err = windows.MoveFileEx(
			fromPtr,
			toPtr,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
		if err == nil {
			return nil
		}
		if !isRetryableWindowsReplaceError(err) {
			return err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("Windows replace remained blocked for %s: %w", windowsReplaceRetryWindow, err)
		}
		sleepFor := delay
		if sleepFor > remaining {
			sleepFor = remaining
		}
		time.Sleep(sleepFor)
		if delay < windowsReplaceMaxRetryDelay {
			delay *= 2
			if delay > windowsReplaceMaxRetryDelay {
				delay = windowsReplaceMaxRetryDelay
			}
		}
	}
}

func isRetryableWindowsReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
