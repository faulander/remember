//go:build darwin

package repository

import "golang.org/x/sys/unix"

func renameFolderNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	return unix.RenameatxNp(oldDir, oldName, newDir, newName, unix.RENAME_EXCL)
}
