//go:build windows

package repository

import "errors"

var errFolderPublicationUnsupported = errors.New("handle-safe folder publication is not supported on Windows")

func RootedFolderIdentity(string, string) (uint64, uint64, error) {
	return 0, 0, errFolderPublicationUnsupported
}
func VerifyRootedFolderEntriesExpected(string, string, uint64, uint64, []string) error {
	return errors.New("handle-safe folder entry verification is not yet supported on Windows")
}

func VerifyRootedEmptyFolderIdentity(string, string, uint64, uint64) error {
	return errors.New("handle-safe empty folder verification is not yet supported on Windows")
}

func VerifyRootedFolderIdentity(string, string, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
func CreateRootedFolderPublication(string, string, [32]byte) (uint64, uint64, error) {
	return 0, 0, errFolderPublicationUnsupported
}
func VerifyRootedFolderPublication(string, string, [32]byte, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
func PublishRootedFolderPublication(string, string, string, [32]byte, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
func CleanupRootedFolderPublication(string, string, [32]byte, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
func RemoveRootedFolderPublicationStage(string, string) error { return errFolderPublicationUnsupported }
func MoveRootedEmptyFolderExpected(string, string, string, uint64, uint64) error {
	return errors.New("handle-safe empty folder move is not yet supported on Windows")
}

func MoveRootedFolderExpected(string, string, string, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
func DeleteRootedFolderExpected(string, string, uint64, uint64) error {
	return errFolderPublicationUnsupported
}
