package app

import (
	"crypto/sha256"
	"testing"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

func TestRecursiveLocalRecoveryDataPreservesDescendantIdentityAndReparentsDirectChildren(t *testing.T) {
	sourceRoot, recoveredRoot := uuid.New(), uuid.New()
	folderA, folderB, sibling := uuid.New(), uuid.New(), uuid.New()
	note, siblingNote := uuid.New(), uuid.New()
	noteHash, siblingHash := sha256.Sum256([]byte("note")), sha256.Sum256([]byte("sibling"))
	manifest := &clientsync.ConflictRecursiveLocalFolderManifest{Members: []clientsync.ConflictRecursiveLocalFolderMember{
		{ObjectID: folderA, ParentID: sourceRoot, ObjectType: clientsync.Folder, Name: "A", RelativePath: "A", Depth: 1, Device: 11, Inode: 21},
		{ObjectID: sibling, ParentID: sourceRoot, ObjectType: clientsync.Folder, Name: "Sibling", RelativePath: "Sibling", Depth: 1, Device: 12, Inode: 22},
		{ObjectID: folderB, ParentID: folderA, ObjectType: clientsync.Folder, Name: "B", RelativePath: "A/B", Depth: 2, Device: 13, Inode: 23},
		{ObjectID: note, ParentID: folderB, ObjectType: clientsync.Note, Name: "Note.md", RelativePath: "A/B/Note.md", Depth: 3, FinalBlobHash: noteHash},
		{ObjectID: siblingNote, ParentID: sibling, ObjectType: clientsync.Note, Name: "Other.md", RelativePath: "Sibling/Other.md", Depth: 2, FinalBlobHash: siblingHash},
	}}
	const target = "_Konflikte/Wiederhergestellt/Root"
	data := recursiveLocalRecoveryData(manifest, target, recoveredRoot)
	if data.folderPaths[recoveredRoot] != target || data.folderPaths[folderA] != target+"/A" || data.folderPaths[folderB] != target+"/A/B" || data.notePaths[note] != target+"/A/B/Note.md" || data.notePaths[siblingNote] != target+"/Sibling/Other.md" {
		t.Fatalf("recovery data=%#v", data)
	}
	objects := map[uuid.UUID]localindex.Object{
		recoveredRoot: {ID: recoveredRoot, Type: localindex.ObjectFolder, RelativePath: target},
		folderA:       {ID: folderA, Type: localindex.ObjectFolder, ParentID: recoveredRoot, RelativePath: target + "/A", FolderDevice: 11, FolderInode: 21},
		sibling:       {ID: sibling, Type: localindex.ObjectFolder, ParentID: recoveredRoot, RelativePath: target + "/Sibling", FolderDevice: 12, FolderInode: 22},
		folderB:       {ID: folderB, Type: localindex.ObjectFolder, ParentID: folderA, RelativePath: target + "/A/B", FolderDevice: 13, FolderInode: 23},
		note:          {ID: note, Type: localindex.ObjectNote, ParentID: folderB, RelativePath: target + "/A/B/Note.md", ContentHash: noteHash[:]},
		siblingNote:   {ID: siblingNote, Type: localindex.ObjectNote, ParentID: sibling, RelativePath: target + "/Sibling/Other.md", ContentHash: siblingHash[:]},
	}
	if !recursiveLocalAlreadyIndexed(objects, manifest, target, sourceRoot, recoveredRoot) {
		t.Fatal("complete recursive recovery index was not recognized")
	}
	replacement := objects[folderA]
	replacement.ParentID = sourceRoot
	objects[folderA] = replacement
	if recursiveLocalAlreadyIndexed(objects, manifest, target, sourceRoot, recoveredRoot) {
		t.Fatal("direct child retained the old root parent")
	}
}
