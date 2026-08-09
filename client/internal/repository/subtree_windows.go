//go:build windows

package repository

import "errors"

func VerifyRootedSubtreeExpected(string, string, uint64, uint64, []RootedSubtreeEntry, int64) error {
	return errors.New("handle-safe recursive subtree verification is not yet supported on Windows")
}
