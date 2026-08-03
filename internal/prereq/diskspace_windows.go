//go:build windows

package prereq

import "errors"

func freeBytes(dir string) (uint64, error) {
	// Template building is Linux-only anyway (see checkOS); disk space on
	// Windows is informational at best, so we don't bother with the
	// syscall.  Returning an error surfaces as a "warn", not a "fail".
	return 0, errors.New("disk space check not implemented on windows")
}
