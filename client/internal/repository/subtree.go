package repository

import "crypto/sha256"

type RootedSubtreeEntryKind string

const (
	RootedSubtreeFolder RootedSubtreeEntryKind = "folder"
	RootedSubtreeFile   RootedSubtreeEntryKind = "file"
)

// RootedSubtreeEntry describes one exact descendant beneath a separately
// identity-bound subtree root. Relative uses slash-separated portable names.
type RootedSubtreeEntry struct {
	Relative      string
	Kind          RootedSubtreeEntryKind
	Device, Inode uint64
	Hash          [sha256.Size]byte
}
