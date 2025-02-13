//go:build windows

package serframe

import (
	"errors"

	"golang.org/x/sys/windows"
)

// requiresTermination returns true, if the provided error value
// other than io.EOF should cause the reader process to terminate.
// On Windows, when a serial USB adapter is removed, an "access denied"
// error may be returned by Read(); in such a case, this function also
// returns true.
func requiresTermination(err error) bool {
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true
	}
	return false
}
