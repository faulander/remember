package app

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"path"
	"strings"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

func prepareRecursiveLocalFolderManifest(root, sourceRelative string, sourceID uuid.UUID, objects map[uuid.UUID]localindex.Object, manifest *clientsync.ConflictRecursiveLocalFolderManifest) error {
	if manifest == nil || len(manifest.Members) == 0 {
		return errors.New("recursive local folder manifest unavailable")
	}
	expected := make(map[uuid.UUID]bool, len(manifest.Members))
	for i := range manifest.Members {
		member := &manifest.Members[i]
		expected[member.ObjectID] = true
		object, ok := objects[member.ObjectID]
		expectedRelative := path.Join(sourceRelative, member.RelativePath)
		if !ok || object.ParentID != member.ParentID || object.RelativePath != expectedRelative || path.Base(object.RelativePath) != member.Name {
			return errors.New("recursive local folder manifest does not match index")
		}
		switch member.ObjectType {
		case clientsync.Folder:
			if object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.FolderDevice == 0 || object.FolderInode == 0 {
				return errors.New("recursive local folder identity unavailable")
			}
			member.Device, member.Inode = object.FolderDevice, object.FolderInode
		case clientsync.Note:
			if object.Type != localindex.ObjectNote || !bytes.Equal(object.ContentHash, member.FinalBlobHash[:]) {
				return errors.New("recursive local note hash mismatch")
			}
			content, err := repository.ReadRooted(root, object.RelativePath, clientsync.MaxBlobBytes)
			if err != nil || sha256.Sum256(content) != member.FinalBlobHash {
				return errors.New("recursive local note bytes mismatch")
			}
			inspection, err := frontmatter.Inspect(content)
			if err != nil || inspection.NoteID != member.ObjectID {
				return errors.New("recursive local note identity mismatch")
			}
		default:
			return errors.New("recursive local folder member type invalid")
		}
	}
	prefix := sourceRelative + "/"
	for _, object := range objects {
		if object.ID == sourceID || !strings.HasPrefix(object.RelativePath, prefix) {
			continue
		}
		if !expected[object.ID] {
			return errors.New("recursive local folder has unmanifested index entry")
		}
	}
	return verifyRecursiveLocalFolder(root, sourceRelative, objects[sourceID].FolderDevice, objects[sourceID].FolderInode, manifest)
}

func verifyRecursiveLocalFolder(root, relative string, device, inode uint64, manifest *clientsync.ConflictRecursiveLocalFolderManifest) error {
	if manifest == nil {
		return errors.New("recursive local folder manifest unavailable")
	}
	entries := make([]repository.RootedSubtreeEntry, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		entry := repository.RootedSubtreeEntry{Relative: member.RelativePath}
		if member.ObjectType == clientsync.Folder {
			entry.Kind, entry.Device, entry.Inode = repository.RootedSubtreeFolder, member.Device, member.Inode
		} else {
			entry.Kind, entry.Hash = repository.RootedSubtreeFile, member.FinalBlobHash
		}
		entries = append(entries, entry)
	}
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, entries, clientsync.MaxBlobBytes)
}

type recursiveLocalReconcileData struct {
	notes       map[uuid.UUID][32]byte
	notePaths   map[uuid.UUID]string
	folders     map[uuid.UUID]bool
	folderPaths map[uuid.UUID]string
	trusted     map[string]uuid.UUID
}

func recursiveLocalRecoveryData(manifest *clientsync.ConflictRecursiveLocalFolderManifest, targetRelative string, recoveredRootID uuid.UUID) recursiveLocalReconcileData {
	data := recursiveLocalReconcileData{
		notes:       map[uuid.UUID][32]byte{},
		notePaths:   map[uuid.UUID]string{},
		folders:     map[uuid.UUID]bool{recoveredRootID: true},
		folderPaths: map[uuid.UUID]string{recoveredRootID: targetRelative},
		trusted:     map[string]uuid.UUID{targetRelative: recoveredRootID},
	}
	for _, member := range manifest.Members {
		relative := path.Join(targetRelative, member.RelativePath)
		if member.ObjectType == clientsync.Folder {
			data.folders[member.ObjectID] = true
			data.folderPaths[member.ObjectID] = relative
			data.trusted[relative] = member.ObjectID
		} else {
			data.notes[member.ObjectID] = member.FinalBlobHash
			data.notePaths[member.ObjectID] = relative
		}
	}
	return data
}

func recursiveLocalAlreadyIndexed(objects map[uuid.UUID]localindex.Object, manifest *clientsync.ConflictRecursiveLocalFolderManifest, targetRelative string, sourceRootID, recoveredRootID uuid.UUID) bool {
	root, ok := objects[recoveredRootID]
	if !ok || root.Type != localindex.ObjectFolder || root.RelativePath != targetRelative {
		return false
	}
	if _, exists := objects[sourceRootID]; exists {
		return false
	}
	for _, member := range manifest.Members {
		object, ok := objects[member.ObjectID]
		expectedParent := member.ParentID
		if member.Depth == 1 {
			expectedParent = recoveredRootID
		}
		if !ok || object.ParentID != expectedParent || object.RelativePath != path.Join(targetRelative, member.RelativePath) {
			return false
		}
		if member.ObjectType == clientsync.Folder {
			if object.Type != localindex.ObjectFolder || object.FolderDevice != member.Device || object.FolderInode != member.Inode {
				return false
			}
		} else if object.Type != localindex.ObjectNote || !bytes.Equal(object.ContentHash, member.FinalBlobHash[:]) {
			return false
		}
	}
	return true
}
