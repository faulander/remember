package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

var testHookBeforeConflictRecoveredPublication func()
var testHookBeforeConflictCompletion func()
var testHookAfterConflictEvacuation func()
var testHookAfterConflictFolderPublication func()
var testHookAfterConflictFolderMoveRevert func()
var testHookAfterConflictFolderMoveReconcile func()
var testHookAfterConflictFolderCreateMove func()
var testHookAfterConflictFolderCreateReconcile func()
var testHookAfterConflictFolderMoveDeleteMove func()
var testHookAfterConflictFolderMoveDeleteReconcile func()
var testHookAfterDivergentFolderEvacuationMove func() error
var testHookAfterDivergentFolderEvacuationReconcile func() error
var testHookBeforeDivergentFolderEvacuatedTransition func() error
var testHookAfterDivergentCanonicalStageCreate func() error
var testHookAfterDivergentCanonicalPublish func() error
var testHookAfterDivergentCanonicalCleanup func() error

var ErrConflictMaterializationActive = errors.New("note conflict materialization is active")

func (c *LocalCore) rejectActiveNoteConflict(ctx context.Context, id uuid.UUID) error {
	store, err := clientsync.NewStore(c.index)
	if err != nil {
		return err
	}
	active, err := store.HasStagedConflictForObject(ctx, id)
	if err != nil {
		return err
	}
	if active {
		return ErrConflictMaterializationActive
	}
	return nil
}

func (c *LocalCore) stageSupportedConflicts(ctx context.Context, store *clientsync.Store, resolver clientsync.BlobResolver) error {
	conflicts, err := store.ListConflicts(ctx, 100)
	if err != nil {
		return err
	}
	for _, conflict := range conflicts {
		m := conflict.Outbox.Mutation
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Create && (conflict.Code == "path_collision" || conflict.Code == "parent_unavailable") && conflict.Canonical == nil {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := c.recoverEmptyFolderCreateCollision(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Move && conflict.Code == "object_deleted" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && conflict.Canonical.Deleted && conflict.Canonical.Revision > m.BaseRevision && len(conflict.Canonical.BlobHash) == 0 {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := c.recoverEmptyFolderMoveAgainstDelete(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Delete && conflict.Code == "base_revision_mismatch" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && !conflict.Canonical.Deleted && conflict.Canonical.Revision > m.BaseRevision && resolver != nil {
			remote, ok := resolver.(interface {
				PreserveAndDeleteEmptyFolder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64) (remotehttp.PreserveDeleteFolderResult, error)
			})
			if ok {
				resolutionID, err := uuid.NewV7()
				if err != nil {
					return err
				}
				resolution, err := store.PrepareFolderPreserveDelete(ctx, m.OperationID, resolutionID)
				if err != nil {
					return err
				}
				if resolution.State == "prepared" {
					result, err := remote.PreserveAndDeleteEmptyFolder(ctx, resolution.ResolutionOperationID, m.OperationID, m.ObjectID, conflict.Canonical.Revision)
					if err != nil {
						var rejected *remotehttp.RejectedError
						if errors.As(err, &rejected) && rejected.Code == "preserve_delete_unavailable" {
							continue
						}
						return err
					}
					if err := store.CompleteFolderPreserveDelete(ctx, m.OperationID, result.RecoveredFolderID, result.RecoveredCursor, result.DeletedCursor); err != nil {
						return err
					}
				}
				continue
			}
		}
		divergentRootFolderMove := m.ObjectType == clientsync.Folder && m.Kind == clientsync.Move && m.ParentID == nil && conflict.Code == "base_revision_mismatch" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && !conflict.Canonical.Deleted && conflict.Canonical.ParentID == nil && conflict.Canonical.Revision > m.BaseRevision && len(conflict.Canonical.BlobHash) == 0 && conflict.Canonical.Name != m.Name
		if divergentRootFolderMove {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := c.recoverEmptyDivergentRootFolderMove(ctx, store, conflict); err != nil {
				if errors.Is(err, clientsync.ErrDivergentFolderMoveIneligible) {
					continue
				}
				return err
			}
			continue
		}
		equivalentFolderMoveRevisionConflict := conflict.Code == "base_revision_mismatch" && conflict.Canonical != nil && conflict.Canonical.Name == m.Name && ((conflict.Canonical.ParentID == nil && m.ParentID == nil) || (conflict.Canonical.ParentID != nil && m.ParentID != nil && *conflict.Canonical.ParentID == *m.ParentID))
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Move && (conflict.Code == "path_collision" || conflict.Code == "parent_unavailable" || conflict.Code == "folder_cycle" || equivalentFolderMoveRevisionConflict) && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && !conflict.Canonical.Deleted {
			if err := c.revertFolderMoveConflict(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Delete && conflict.Code == "folder_not_empty" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && !conflict.Canonical.Deleted {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := c.restoreFolderNotEmptyConflict(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		alreadyDeleted := m.Kind == clientsync.Delete && ((conflict.Code == "object_missing" && conflict.Canonical == nil) || (conflict.Code == "object_deleted" && conflict.Canonical != nil && conflict.Canonical.ObjectType == m.ObjectType && conflict.Canonical.Deleted && conflict.Canonical.Revision > m.BaseRevision))
		if alreadyDeleted {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := store.ResolveMissingDelete(ctx, m.OperationID); err != nil {
				return err
			}
			continue
		}
		equivalentNoteMove := m.ObjectType == clientsync.Note && m.Kind == clientsync.Move && conflict.Code == "base_revision_mismatch" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Note && !conflict.Canonical.Deleted && conflict.Canonical.Name == m.Name && conflict.Canonical.Revision > m.BaseRevision && len(conflict.Canonical.BlobHash) == sha256.Size && (m.ParentID == nil && conflict.Canonical.ParentID == nil || m.ParentID != nil && conflict.Canonical.ParentID != nil && *m.ParentID == *conflict.Canonical.ParentID)
		if equivalentNoteMove {
			if err := c.resolveEquivalentNoteMove(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		canonicalAbsent := conflict.Canonical == nil && ((m.Kind == clientsync.Create && (conflict.Code == "path_collision" || conflict.Code == "parent_unavailable")) || ((m.Kind == clientsync.Update || m.Kind == clientsync.Move) && conflict.Code == "object_missing"))
		revisionMoveCollision := m.Kind == clientsync.Move && conflict.Code == "base_revision_mismatch" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Note && !conflict.Canonical.Deleted && len(conflict.Canonical.BlobHash) == sha256.Size && conflict.Canonical.Revision > m.BaseRevision && conflict.Canonical.Name != m.Name && (conflict.Canonical.ParentID == nil && m.ParentID == nil || conflict.Canonical.ParentID != nil && m.ParentID != nil && *conflict.Canonical.ParentID == *m.ParentID)
		moveCollision := m.Kind == clientsync.Move && (conflict.Code == "path_collision" || conflict.Code == "parent_unavailable") && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Note && !conflict.Canonical.Deleted && len(conflict.Canonical.BlobHash) == sha256.Size || revisionMoveCollision
		if m.ObjectType == clientsync.Note && (canonicalAbsent || moveCollision) {
			if revisionMoveCollision && m.ParentID != nil {
				if err := c.validateDivergentNestedNoteMove(ctx, store, m); err != nil {
					return err
				}
			}
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if canonicalAbsent {
				if err := c.stageNoteCanonicalAbsentConflict(ctx, store, conflict); err != nil {
					return err
				}
			} else if err := c.stageNoteMovePathCollision(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		if conflict.Canonical == nil || m.ObjectType != clientsync.Note || conflict.Canonical.ObjectType != clientsync.Note || len(conflict.Canonical.BlobHash) != sha256.Size || conflict.Canonical.Revision <= m.BaseRevision {
			continue
		}
		localWrite := (m.Kind == clientsync.Update && ((conflict.Code == "base_revision_mismatch" && !conflict.Canonical.Deleted) || (conflict.Code == "object_deleted" && conflict.Canonical.Deleted))) || (m.Kind == clientsync.Move && conflict.Code == "object_deleted" && conflict.Canonical.Deleted)
		localDelete := m.Kind == clientsync.Delete && conflict.Code == "base_revision_mismatch" && !conflict.Canonical.Deleted
		if !localWrite && !localDelete {
			continue
		}
		if localDelete && resolver == nil {
			continue
		}
		if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
			return err
		}
		if localWrite {
			if err := c.stageNoteUpdateConflict(ctx, store, conflict); err != nil {
				return err
			}
		} else if err := c.stageNoteDeleteConflict(ctx, store, resolver, conflict); err != nil {
			return err
		}
	}
	return nil
}

func (c *LocalCore) resolveEquivalentNoteMove(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m := conflict.Outbox.Mutation
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	if m.ParentID != nil {
		parent, ok := objects[*m.ParentID]
		if !ok || parent.Type != localindex.ObjectFolder || parent.IdentityState != localindex.IdentityKnown || parent.FolderDevice == 0 || parent.FolderInode == 0 {
			return errors.New("equivalent note move parent identity unavailable")
		}
		unresolved, err := store.HasUnresolvedLocalIntent(ctx, *m.ParentID)
		if err != nil {
			return err
		}
		if unresolved {
			return errors.New("equivalent note move parent has unresolved local intent")
		}
		if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
			return err
		}
	}
	target, err := remoteNotePath(objects, m.ParentID, m.Name)
	if err != nil {
		return err
	}
	found, ok := objects[m.ObjectID]
	if !ok || found.Type != localindex.ObjectNote || found.RelativePath != target {
		return errors.New("equivalent note move local target mismatch")
	}
	document, err := c.readNoteFileRooted(target)
	if err != nil {
		return err
	}
	if document.ID != m.ObjectID {
		return errors.New("equivalent note move file identity mismatch")
	}
	return store.ResolveEquivalentNoteMove(ctx, m.OperationID)
}

func (c *LocalCore) validateDivergentNestedNoteMove(ctx context.Context, store *clientsync.Store, m clientsync.Mutation) error {
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	parent, ok := objects[*m.ParentID]
	if !ok || parent.Type != localindex.ObjectFolder || parent.IdentityState != localindex.IdentityKnown || parent.FolderDevice == 0 || parent.FolderInode == 0 {
		return errors.New("divergent nested note move parent identity unavailable")
	}
	unresolved, err := store.HasUnresolvedLocalIntent(ctx, *m.ParentID)
	if err != nil {
		return err
	}
	if unresolved {
		return errors.New("divergent nested note move parent has unresolved local intent")
	}
	if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
		return err
	}
	target, err := remoteNotePath(objects, m.ParentID, m.Name)
	if err != nil {
		return err
	}
	object, ok := objects[m.ObjectID]
	if !ok || object.Type != localindex.ObjectNote || object.IdentityState != localindex.IdentityKnown || object.RelativePath != target {
		return errors.New("divergent nested note move local target mismatch")
	}
	content, err := repository.ReadRooted(c.root, target, clientsync.MaxBlobBytes)
	if err != nil {
		return err
	}
	inspection, err := frontmatter.Inspect(content)
	if err != nil || inspection.NoteID != m.ObjectID {
		return errors.New("divergent nested note move file identity mismatch")
	}
	return nil
}

func (c *LocalCore) recoverEmptyFolderMoveAgainstDelete(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m, canonical := conflict.Outbox.Mutation, conflict.Canonical
	recovery, err := store.ConflictFolderMoveDeleteRecovery(ctx, m.OperationID)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	var members []clientsync.ConflictFolderCreateNoteMember
	if recovery == nil {
		object, ok := objects[m.ObjectID]
		if !ok || object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.FolderDevice == 0 || object.FolderInode == 0 {
			return errors.New("moved deleted folder identity unavailable")
		}
		attempted, err := remoteNotePath(objects, m.ParentID, m.Name)
		if err != nil {
			return err
		}
		if attempted != object.RelativePath {
			return errors.New("moved deleted folder path mismatch")
		}
		candidates, err := store.PendingDirectNoteCreates(ctx, m.OperationID, m.ObjectID)
		if err != nil {
			return err
		}
		byID := make(map[uuid.UUID]clientsync.ConflictFolderCreateNoteMember, len(candidates))
		for _, candidate := range candidates {
			byID[candidate.NoteID] = candidate
		}
		prefix := attempted + "/"
		for _, child := range snapshot.Objects {
			if child.ID == object.ID || !strings.HasPrefix(child.RelativePath, prefix) {
				continue
			}
			if child.Type != localindex.ObjectNote || path.Dir(child.RelativePath) != attempted {
				return errors.New("folder move/delete recovery supports direct notes only")
			}
			candidate, ok := byID[child.ID]
			if !ok || candidate.Name != path.Base(child.RelativePath) || !bytes.Equal(child.ContentHash, candidate.BlobHash[:]) {
				return errors.New("folder move/delete direct note intent mismatch")
			}
			content, readErr := repository.ReadRooted(c.root, child.RelativePath, clientsync.MaxBlobBytes)
			if readErr != nil || sha256.Sum256(content) != candidate.BlobHash {
				return errors.New("folder move/delete direct note bytes mismatch")
			}
			inspection, inspectErr := frontmatter.Inspect(content)
			if inspectErr != nil || inspection.NoteID != child.ID {
				return errors.New("folder move/delete direct note identity mismatch")
			}
			members = append(members, candidate)
			delete(byID, child.ID)
		}
		if len(byID) != 0 {
			return errors.New("folder move/delete note manifest incomplete")
		}
		if len(members) == 0 {
			if err := repository.VerifyRootedEmptyFolderIdentity(c.root, attempted, object.FolderDevice, object.FolderInode); err != nil {
				return err
			}
		} else if err := verifyDirectNoteFolder(c.root, attempted, object.FolderDevice, object.FolderInode, members); err != nil {
			return err
		}
		recoveredID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		newOperationID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		target := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFolderName(path.Base(attempted), m.OperationID)
		recovery = &clientsync.ConflictFolderMoveDeleteRecovery{OperationID: m.OperationID, FolderID: m.ObjectID, RecoveredFolderID: recoveredID, NewOperationID: newOperationID, AttemptedRelative: attempted, TargetRelative: target, Device: object.FolderDevice, Inode: object.FolderInode, CanonicalRevision: canonical.Revision, State: "prepared"}
		if len(members) == 0 {
			err = store.PutConflictFolderMoveDeleteRecovery(ctx, *recovery)
		} else {
			err = store.PutConflictFolderMoveDeleteRecoveryWithNotes(ctx, *recovery, members)
		}
		if err != nil {
			return err
		}
	} else {
		members, err = store.ConflictFolderMoveDeleteNoteMembers(ctx, m.OperationID)
		if err != nil {
			return err
		}
	}
	parent, ok := objects[clientsync.ConflictRecoveredID]
	if !ok || parent.Type != localindex.ObjectFolder || parent.IdentityState != localindex.IdentityKnown || parent.FolderDevice == 0 || parent.FolderInode == 0 {
		return errors.New("recovered conflict parent identity unavailable")
	}
	verify := func() error {
		if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
			return err
		}
		if len(members) == 0 {
			return repository.VerifyRootedEmptyFolderIdentity(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode)
		}
		return verifyDirectNoteFolder(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode, members)
	}
	if recovery.State == "prepared" {
		if targetErr := verify(); targetErr != nil {
			if identityErr := repository.VerifyRootedFolderIdentity(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode); identityErr == nil {
				if restoreErr := repository.MoveRootedFolderExpected(c.root, recovery.TargetRelative, recovery.AttemptedRelative, recovery.Device, recovery.Inode); restoreErr != nil {
					return errors.Join(targetErr, restoreErr)
				}
				return targetErr
			}
			var sourceErr error
			if len(members) == 0 {
				sourceErr = repository.VerifyRootedEmptyFolderIdentity(c.root, recovery.AttemptedRelative, recovery.Device, recovery.Inode)
			} else {
				sourceErr = verifyDirectNoteFolder(c.root, recovery.AttemptedRelative, recovery.Device, recovery.Inode, members)
			}
			if sourceErr != nil {
				return sourceErr
			}
			var moveErr error
			if len(members) == 0 {
				moveErr = repository.MoveRootedEmptyFolderExpected(c.root, recovery.AttemptedRelative, recovery.TargetRelative, recovery.Device, recovery.Inode)
			} else {
				moveErr = repository.MoveRootedFolderExpected(c.root, recovery.AttemptedRelative, recovery.TargetRelative, recovery.Device, recovery.Inode)
			}
			if moveErr != nil {
				return moveErr
			}
		}
		if testHookAfterConflictFolderMoveDeleteMove != nil {
			testHookAfterConflictFolderMoveDeleteMove()
		}
		indexed, alreadyIndexed := objects[recovery.RecoveredFolderID]
		_, oldExists := objects[recovery.FolderID]
		alreadyIndexed = alreadyIndexed && indexed.Type == localindex.ObjectFolder && indexed.RelativePath == recovery.TargetRelative && !oldExists
		if alreadyIndexed {
			for _, member := range members {
				note, exists := objects[member.NoteID]
				if !exists || note.Type != localindex.ObjectNote || note.ParentID != recovery.RecoveredFolderID || note.RelativePath != path.Join(recovery.TargetRelative, member.Name) || !bytes.Equal(note.ContentHash, member.BlobHash[:]) {
					alreadyIndexed = false
					break
				}
			}
		}
		appliedNotes := map[uuid.UUID][32]byte{}
		appliedPaths := map[uuid.UUID]string{}
		for _, member := range members {
			appliedNotes[member.NoteID] = member.BlobHash
			appliedPaths[member.NoteID] = path.Join(recovery.TargetRelative, member.Name)
		}
		if !alreadyIndexed {
			if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteDeletes: map[uuid.UUID]bool{recovery.FolderID: true}, AppliedRemoteNotes: appliedNotes, AppliedRemoteNotePaths: appliedPaths, TrustedRemoteFolderDeletes: map[string]uuid.UUID{recovery.AttemptedRelative: recovery.FolderID}, TrustedRemoteFolders: map[string]uuid.UUID{recovery.TargetRelative: recovery.RecoveredFolderID}, VerifyTrustedRemoteFolders: verify}); err != nil {
				return err
			}
		}
		if err := verify(); err != nil {
			return err
		}
		if testHookAfterConflictFolderMoveDeleteReconcile != nil {
			testHookAfterConflictFolderMoveDeleteReconcile()
		}
		if err := store.MarkConflictFolderMoveDeleteMoved(ctx, m.OperationID); err != nil {
			return err
		}
		recovery.State = "moved"
	}
	if recovery.State == "moved" {
		if err := verify(); err != nil {
			return err
		}
		revision, found, err := store.Baseline(ctx, recovery.FolderID)
		if err != nil {
			return err
		}
		if !found || revision != recovery.CanonicalRevision {
			return nil
		}
		_, parentFound, err := store.Baseline(ctx, clientsync.ConflictRecoveredID)
		if err != nil {
			return err
		}
		if !parentFound {
			return nil
		}
		if err := store.CompleteConflictFolderMoveDeleteRecovery(ctx, *recovery); err != nil {
			return err
		}
		recovery.State = "completed"
	}
	return verify()
}

func verifyDivergentDirectNoteSubtree(root, relative string, device, inode uint64, members []clientsync.ConflictFolderCreateNoteMember) error {
	entries := make([]repository.RootedSubtreeEntry, 0, len(members))
	for _, member := range members {
		entries = append(entries, repository.RootedSubtreeEntry{Relative: member.Name, Kind: repository.RootedSubtreeFile, Hash: member.BlobHash})
	}
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, entries, clientsync.MaxBlobBytes)
}

func (c *LocalCore) recoverEmptyDivergentRootFolderMove(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m, canonical := conflict.Outbox.Mutation, conflict.Canonical
	recovery, err := store.ConflictFolderDivergentMoveRecovery(ctx, m.OperationID)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	paths := make(map[string]bool, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
		paths[portablePathKey(object.RelativePath)] = true
	}
	var members []clientsync.ConflictFolderCreateNoteMember
	if recovery == nil {
		eligible, err := store.DivergentFolderMoveRecoveryEligible(ctx, m.OperationID)
		if err != nil {
			return err
		}
		if !eligible {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		object, ok := objects[m.ObjectID]
		if !ok || object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.ParentID != uuid.Nil || object.RelativePath != m.Name || object.FolderDevice == 0 || object.FolderInode == 0 {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		if paths[portablePathKey(canonical.Name)] {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		if _, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(canonical.Name))); err == nil || !os.IsNotExist(err) {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		members, err = store.PendingDirectNoteCreates(ctx, m.OperationID, m.ObjectID)
		if err != nil {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		byID := map[uuid.UUID]clientsync.ConflictFolderCreateNoteMember{}
		for _, member := range members {
			byID[member.NoteID] = member
		}
		prefix := object.RelativePath + "/"
		for _, desc := range snapshot.Objects {
			if desc.ID == object.ID || !strings.HasPrefix(desc.RelativePath, prefix) {
				continue
			}
			member, ok := byID[desc.ID]
			if !ok || desc.Type != localindex.ObjectNote || path.Dir(desc.RelativePath) != object.RelativePath || member.Name != path.Base(desc.RelativePath) || !bytes.Equal(desc.ContentHash, member.BlobHash[:]) {
				return clientsync.ErrDivergentFolderMoveIneligible
			}
			delete(byID, desc.ID)
		}
		if len(byID) != 0 {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		if err := verifyDivergentDirectNoteSubtree(c.root, object.RelativePath, object.FolderDevice, object.FolderInode, members); err != nil {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		recoveredID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		newOperation, err := uuid.NewV7()
		if err != nil {
			return err
		}
		target := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFolderName(path.Base(object.RelativePath), m.OperationID)
		if paths[portablePathKey(target)] {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		if _, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(target))); err == nil || !os.IsNotExist(err) {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		var nonce [sha256.Size]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		recovery = &clientsync.ConflictFolderDivergentMoveRecovery{OperationID: m.OperationID, FolderID: m.ObjectID, RecoveredFolderID: recoveredID, NewOperationID: newOperation, AttemptedRelative: object.RelativePath, CanonicalRelative: canonical.Name, RecoveryRelative: target, SourceDevice: object.FolderDevice, SourceInode: object.FolderInode, CanonicalRevision: canonical.Revision, CanonicalNonce: nonce, State: "prepared"}
		if len(members) == 0 {
			err = store.PutConflictFolderDivergentMoveRecovery(ctx, *recovery)
		} else {
			err = store.PutConflictFolderDivergentMoveRecoveryWithNotes(ctx, *recovery, members)
		}
		if err != nil {
			return err
		}
	} else {
		members, err = store.ConflictFolderDivergentMoveNoteMembers(ctx, m.OperationID)
		if err != nil {
			return err
		}
	}
	verifyRecovery := func() error {
		return verifyDivergentDirectNoteSubtree(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, members)
	}
	noteOptions := func(base string) (map[uuid.UUID][32]byte, map[uuid.UUID]string) {
		hashes := map[uuid.UUID][32]byte{}
		paths := map[uuid.UUID]string{}
		for _, member := range members {
			hashes[member.NoteID] = member.BlobHash
			paths[member.NoteID] = path.Join(base, member.Name)
		}
		return hashes, paths
	}
	recoveryNotes, recoveryPaths := noteOptions(recovery.RecoveryRelative)
	attemptedNotes, attemptedPaths := noteOptions(recovery.AttemptedRelative)
	restorePrepared := func(cause error) error {
		restoreErr := repository.MoveRootedFolderExpected(c.root, recovery.RecoveryRelative, recovery.AttemptedRelative, recovery.SourceDevice, recovery.SourceInode)
		if restoreErr == nil {
			_, restoreErr = reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: attemptedNotes, AppliedRemoteNotePaths: attemptedPaths, AppliedRemoteFolders: map[uuid.UUID]bool{recovery.FolderID: true}, AppliedRemoteFolderPaths: map[uuid.UUID]string{recovery.FolderID: recovery.AttemptedRelative}, TrustedRemoteFolders: map[string]uuid.UUID{recovery.AttemptedRelative: recovery.FolderID}, VerifyTrustedRemoteFolders: func() error {
				return verifyDivergentDirectNoteSubtree(c.root, recovery.AttemptedRelative, recovery.SourceDevice, recovery.SourceInode, members)
			}})
		}
		return errors.Join(cause, restoreErr)
	}
	if recovery.State == "prepared" {
		if err := verifyRecovery(); err != nil {
			if err := verifyDivergentDirectNoteSubtree(c.root, recovery.AttemptedRelative, recovery.SourceDevice, recovery.SourceInode, members); err != nil {
				return err
			}
			if err := repository.MoveRootedFolderExpected(c.root, recovery.AttemptedRelative, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode); err != nil {
				return err
			}
		}
		if testHookAfterDivergentFolderEvacuationMove != nil {
			if err := testHookAfterDivergentFolderEvacuationMove(); err != nil {
				return restorePrepared(err)
			}
		}
		verify := func() error { return verifyRecovery() }
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteDeletes: map[uuid.UUID]bool{recovery.FolderID: true}, AppliedRemoteNotes: recoveryNotes, AppliedRemoteNotePaths: recoveryPaths, AppliedRemoteFolders: map[uuid.UUID]bool{recovery.RecoveredFolderID: true}, AppliedRemoteFolderPaths: map[uuid.UUID]string{recovery.RecoveredFolderID: recovery.RecoveryRelative}, TrustedRemoteFolderDeletes: map[string]uuid.UUID{recovery.AttemptedRelative: recovery.FolderID}, TrustedRemoteFolders: map[string]uuid.UUID{recovery.RecoveryRelative: recovery.RecoveredFolderID}, VerifyTrustedRemoteFolders: verify}); err != nil {
			return restorePrepared(err)
		}
		if testHookAfterDivergentFolderEvacuationReconcile != nil {
			if err := testHookAfterDivergentFolderEvacuationReconcile(); err != nil {
				return restorePrepared(err)
			}
		}
		if testHookBeforeDivergentFolderEvacuatedTransition != nil {
			if err := testHookBeforeDivergentFolderEvacuatedTransition(); err != nil {
				return restorePrepared(err)
			}
		}
		if err := store.MarkConflictFolderDivergentMoveEvacuated(ctx, recovery.OperationID); err != nil {
			return restorePrepared(err)
		}
		recovery.State = "evacuated"
	}
	stage := ".remember/conflicts/folders/" + recovery.OperationID.String()
	if recovery.State == "evacuated" {
		if err := verifyRecovery(); err != nil {
			return err
		}
		device, inode, err := repository.RootedFolderIdentity(c.root, stage)
		if err == nil {
			err = repository.VerifyRootedFolderPublication(c.root, stage, recovery.CanonicalNonce, device, inode)
		} else {
			device, inode, err = repository.CreateRootedFolderPublication(c.root, stage, recovery.CanonicalNonce)
		}
		if err != nil {
			return err
		}
		if testHookAfterDivergentCanonicalStageCreate != nil {
			if err := testHookAfterDivergentCanonicalStageCreate(); err != nil {
				return err
			}
		}
		if err := store.MarkConflictFolderDivergentMoveCanonicalPrepared(ctx, recovery.OperationID, device, inode); err != nil {
			return err
		}
		recovery.State = "canonical_prepared"
		recovery.CanonicalDevice = device
		recovery.CanonicalInode = inode
	}
	if recovery.State == "canonical_prepared" {
		stageErr := repository.VerifyRootedFolderPublication(c.root, stage, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode)
		if stageErr == nil {
			if err := repository.PublishRootedFolderPublication(c.root, stage, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err != nil {
				return err
			}
		} else if targetErr := repository.VerifyRootedFolderPublication(c.root, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); targetErr != nil {
			return errors.Join(stageErr, targetErr)
		}
		if testHookAfterDivergentCanonicalPublish != nil {
			if err := testHookAfterDivergentCanonicalPublish(); err != nil {
				return err
			}
		}
		if err := store.MarkConflictFolderDivergentMoveCanonicalPublished(ctx, recovery.OperationID); err != nil {
			return err
		}
		recovery.State = "canonical_published"
	}
	if recovery.State == "canonical_published" {
		if err := repository.CleanupRootedFolderPublication(c.root, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err != nil {
			return err
		}
		if testHookAfterDivergentCanonicalCleanup != nil {
			if err := testHookAfterDivergentCanonicalCleanup(); err != nil {
				return err
			}
		}
		verify := func() error {
			if err := verifyRecovery(); err != nil {
				return err
			}
			return repository.VerifyRootedSubtreeExpected(c.root, recovery.CanonicalRelative, recovery.CanonicalDevice, recovery.CanonicalInode, nil, clientsync.MaxBlobBytes)
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: recoveryNotes, AppliedRemoteNotePaths: recoveryPaths, AppliedRemoteFolders: map[uuid.UUID]bool{recovery.FolderID: true, recovery.RecoveredFolderID: true}, AppliedRemoteFolderPaths: map[uuid.UUID]string{recovery.FolderID: recovery.CanonicalRelative, recovery.RecoveredFolderID: recovery.RecoveryRelative}, TrustedRemoteFolders: map[string]uuid.UUID{recovery.RecoveryRelative: recovery.RecoveredFolderID, recovery.CanonicalRelative: recovery.FolderID}, VerifyTrustedRemoteFolders: verify}); err != nil {
			return err
		}
		if err := verifyRecovery(); err != nil {
			return err
		}
		if err := repository.VerifyRootedSubtreeExpected(c.root, recovery.CanonicalRelative, recovery.CanonicalDevice, recovery.CanonicalInode, nil, clientsync.MaxBlobBytes); err != nil {
			return err
		}
		revision, found, err := store.Baseline(ctx, recovery.FolderID)
		if err != nil {
			return err
		}
		if !found || revision != recovery.CanonicalRevision {
			return nil
		}
		if _, found, err := store.Baseline(ctx, clientsync.ConflictRecoveredID); err != nil {
			return err
		} else if !found {
			return nil
		}
		if err := store.CompleteConflictFolderDivergentMoveRecovery(ctx, *recovery); err != nil {
			return err
		}
		recovery.State = "completed"
	}
	return verifyRecovery()
}

func (c *LocalCore) recoverEmptyFolderCreateCollision(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m := conflict.Outbox.Mutation
	recovery, err := store.ConflictFolderCreateRecovery(ctx, m.OperationID)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	var members []clientsync.ConflictFolderCreateNoteMember
	if recovery == nil {
		source, exists := objects[m.ObjectID]
		if !exists || source.Type != localindex.ObjectFolder || source.IdentityState != localindex.IdentityKnown || source.FolderDevice == 0 || source.FolderInode == 0 {
			return errors.New("folder create collision source identity unavailable")
		}
		candidates, err := store.PendingDirectNoteCreates(ctx, m.OperationID, m.ObjectID)
		if err != nil {
			return err
		}
		byID := make(map[uuid.UUID]clientsync.ConflictFolderCreateNoteMember, len(candidates))
		for _, candidate := range candidates {
			byID[candidate.NoteID] = candidate
		}
		prefix := source.RelativePath + "/"
		for _, object := range snapshot.Objects {
			if object.ID == source.ID || !strings.HasPrefix(object.RelativePath, prefix) {
				continue
			}
			if object.Type != localindex.ObjectNote || path.Dir(object.RelativePath) != source.RelativePath {
				return errors.New("folder create recovery supports direct notes only")
			}
			candidate, ok := byID[object.ID]
			if !ok || candidate.Name != path.Base(object.RelativePath) || !bytes.Equal(object.ContentHash, candidate.BlobHash[:]) {
				return errors.New("folder create direct note intent mismatch")
			}
			content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
			if err != nil || sha256.Sum256(content) != candidate.BlobHash {
				return errors.New("folder create direct note bytes mismatch")
			}
			inspection, err := frontmatter.Inspect(content)
			if err != nil || inspection.NoteID != object.ID {
				return errors.New("folder create direct note identity mismatch")
			}
			members = append(members, candidate)
			delete(byID, object.ID)
		}
		if len(byID) != 0 {
			return errors.New("folder create direct note manifest incomplete")
		}
		recoveredID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		target := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFolderName(path.Base(source.RelativePath), m.OperationID)
		recovery = &clientsync.ConflictFolderCreateRecovery{OperationID: m.OperationID, SourceFolderID: m.ObjectID, RecoveredFolderID: recoveredID, SourceRelative: source.RelativePath, TargetRelative: target, Device: source.FolderDevice, Inode: source.FolderInode, State: "prepared"}
		if len(members) == 0 {
			if err := repository.VerifyRootedEmptyFolderIdentity(c.root, source.RelativePath, source.FolderDevice, source.FolderInode); err != nil {
				return err
			}
			if err := store.PutConflictFolderCreateRecovery(ctx, *recovery); err != nil {
				return err
			}
		} else {
			if err := verifyDirectNoteFolder(c.root, source.RelativePath, source.FolderDevice, source.FolderInode, members); err != nil {
				return err
			}
			if err := store.PutConflictFolderCreateRecoveryWithNotes(ctx, *recovery, members); err != nil {
				return err
			}
		}
	} else {
		members, err = store.ConflictFolderCreateNoteMembers(ctx, m.OperationID)
		if err != nil {
			return err
		}
	}
	recoveredParent, ok := objects[clientsync.ConflictRecoveredID]
	if !ok || recoveredParent.Type != localindex.ObjectFolder || recoveredParent.IdentityState != localindex.IdentityKnown || recoveredParent.FolderDevice == 0 || recoveredParent.FolderInode == 0 {
		return errors.New("recovered conflict parent identity unavailable")
	}
	verify := func() error {
		if err := repository.VerifyRootedFolderIdentity(c.root, recoveredParent.RelativePath, recoveredParent.FolderDevice, recoveredParent.FolderInode); err != nil {
			return err
		}
		if len(members) == 0 {
			return repository.VerifyRootedEmptyFolderIdentity(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode)
		}
		return verifyDirectNoteFolder(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode, members)
	}
	if recovery.State == "prepared" {
		if targetErr := verify(); targetErr != nil {
			if identityErr := repository.VerifyRootedFolderIdentity(c.root, recovery.TargetRelative, recovery.Device, recovery.Inode); identityErr == nil {
				if restoreErr := repository.MoveRootedFolderExpected(c.root, recovery.TargetRelative, recovery.SourceRelative, recovery.Device, recovery.Inode); restoreErr != nil {
					return errors.Join(targetErr, fmt.Errorf("restore changed folder recovery target: %w", restoreErr))
				}
				return targetErr
			}
			var sourceErr error
			if len(members) == 0 {
				sourceErr = repository.VerifyRootedEmptyFolderIdentity(c.root, recovery.SourceRelative, recovery.Device, recovery.Inode)
			} else {
				sourceErr = verifyDirectNoteFolder(c.root, recovery.SourceRelative, recovery.Device, recovery.Inode, members)
			}
			if sourceErr != nil {
				return sourceErr
			}
			var moveErr error
			if len(members) == 0 {
				moveErr = repository.MoveRootedEmptyFolderExpected(c.root, recovery.SourceRelative, recovery.TargetRelative, recovery.Device, recovery.Inode)
			} else {
				moveErr = repository.MoveRootedFolderExpected(c.root, recovery.SourceRelative, recovery.TargetRelative, recovery.Device, recovery.Inode)
			}
			if moveErr != nil {
				return moveErr
			}
		}
		if testHookAfterConflictFolderCreateMove != nil {
			testHookAfterConflictFolderCreateMove()
		}
		alreadyIndexed := false
		if indexed, ok := objects[recovery.RecoveredFolderID]; ok && indexed.Type == localindex.ObjectFolder && indexed.RelativePath == recovery.TargetRelative {
			_, oldExists := objects[recovery.SourceFolderID]
			alreadyIndexed = !oldExists
			for _, member := range members {
				note, exists := objects[member.NoteID]
				if !exists || note.Type != localindex.ObjectNote || note.ParentID != recovery.RecoveredFolderID || note.RelativePath != path.Join(recovery.TargetRelative, member.Name) || !bytes.Equal(note.ContentHash, member.BlobHash[:]) {
					alreadyIndexed = false
					break
				}
			}
		}
		appliedNotes := map[uuid.UUID][32]byte{}
		appliedPaths := map[uuid.UUID]string{}
		for _, member := range members {
			appliedNotes[member.NoteID] = member.BlobHash
			appliedPaths[member.NoteID] = path.Join(recovery.TargetRelative, member.Name)
		}
		if !alreadyIndexed {
			if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteDeletes: map[uuid.UUID]bool{recovery.SourceFolderID: true}, AppliedRemoteNotes: appliedNotes, AppliedRemoteNotePaths: appliedPaths, TrustedRemoteFolderDeletes: map[string]uuid.UUID{recovery.SourceRelative: recovery.SourceFolderID}, TrustedRemoteFolders: map[string]uuid.UUID{recovery.TargetRelative: recovery.RecoveredFolderID}, VerifyTrustedRemoteFolders: verify}); err != nil {
				return err
			}
		}
		if err := verify(); err != nil {
			return err
		}
		if testHookAfterConflictFolderCreateReconcile != nil {
			testHookAfterConflictFolderCreateReconcile()
		}
		if err := store.MarkConflictFolderCreateMoved(ctx, m.OperationID); err != nil {
			return err
		}
		recovery.State = "moved"
	}
	if recovery.State == "moved" {
		if err := verify(); err != nil {
			return err
		}
		_, _, durable, err := store.Projection(ctx, clientsync.ConflictRecoveredID)
		if err != nil {
			return err
		}
		if !durable {
			return nil
		}
		if err := store.CompleteConflictFolderCreateRecovery(ctx, *recovery); err != nil {
			return err
		}
		recovery.State = "completed"
	}
	return verify()
}

func verifyDirectNoteFolder(root, relative string, device, inode uint64, members []clientsync.ConflictFolderCreateNoteMember) error {
	names := make([]string, 0, len(members))
	for _, member := range members {
		names = append(names, member.Name)
	}
	if err := repository.VerifyRootedFolderEntriesExpected(root, relative, device, inode, names); err != nil {
		return err
	}
	inventory, err := reconcile.Scan(root)
	if err != nil {
		return err
	}
	expected := make(map[string]clientsync.ConflictFolderCreateNoteMember, len(members))
	for _, member := range members {
		expected[path.Join(relative, member.Name)] = member
	}
	folderSeen := false
	for _, issue := range inventory.Issues {
		if issue.RelativePath == relative || strings.HasPrefix(issue.RelativePath, relative+"/") {
			return errors.New("folder recovery subtree has scan issue")
		}
	}
	for _, entry := range inventory.Entries {
		if entry.RelativePath == relative {
			if entry.Type != reconcile.EntryFolder {
				return errors.New("folder recovery root mismatch")
			}
			folderSeen = true
			continue
		}
		if !strings.HasPrefix(entry.RelativePath, relative+"/") {
			continue
		}
		member, ok := expected[entry.RelativePath]
		if !ok || entry.Type != reconcile.EntryNote || entry.NoteID != member.NoteID || entry.ContentHash != member.BlobHash {
			return errors.New("folder recovery manifest mismatch")
		}
		delete(expected, entry.RelativePath)
	}
	if !folderSeen || len(expected) != 0 {
		return errors.New("folder recovery manifest incomplete")
	}
	return nil
}

func (c *LocalCore) revertFolderMoveConflict(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m, canonical := conflict.Outbox.Mutation, conflict.Canonical
	validRevision := canonical.Revision == m.BaseRevision && conflict.Code != "base_revision_mismatch" || canonical.Revision > m.BaseRevision && conflict.Code == "base_revision_mismatch"
	equivalentRevisionMove := conflict.Code != "base_revision_mismatch" || canonical.Name == m.Name && ((canonical.ParentID == nil && m.ParentID == nil) || (canonical.ParentID != nil && m.ParentID != nil && *canonical.ParentID == *m.ParentID))
	if !validRevision || !equivalentRevisionMove || canonical.BlobHash != nil {
		return errors.New("folder move conflict canonical state mismatch")
	}
	revert, err := store.ConflictFolderMoveRevert(ctx, m.OperationID)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	attempted, err := remoteNotePath(objects, m.ParentID, m.Name)
	if err != nil {
		return err
	}
	canonicalPath, err := remoteNotePath(objects, canonical.ParentID, canonical.Name)
	if err != nil {
		return err
	}
	object, exists := objects[m.ObjectID]
	if !exists || object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.FolderDevice == 0 || object.FolderInode == 0 {
		return errors.New("moved conflict folder identity unavailable")
	}
	if revert == nil {
		revert = &clientsync.ConflictFolderMoveRevert{OperationID: m.OperationID, FolderID: m.ObjectID, AttemptedRelative: attempted, CanonicalRelative: canonicalPath, Device: object.FolderDevice, Inode: object.FolderInode, State: "prepared"}
		if err := store.PutConflictFolderMoveRevert(ctx, *revert); err != nil {
			return err
		}
	}
	if revert.FolderID != m.ObjectID || revert.AttemptedRelative != attempted || revert.CanonicalRelative != canonicalPath || revert.Device != object.FolderDevice || revert.Inode != object.FolderInode {
		return errors.New("folder move revert identity mismatch")
	}
	needsReconcile := revert.AttemptedRelative != revert.CanonicalRelative && object.RelativePath == revert.AttemptedRelative
	if object.RelativePath != revert.AttemptedRelative && object.RelativePath != revert.CanonicalRelative {
		return errors.New("folder move revert snapshot path mismatch")
	}
	verify := func() error {
		return repository.VerifyRootedFolderIdentity(c.root, revert.CanonicalRelative, revert.Device, revert.Inode)
	}
	if revert.State == "prepared" {
		if verify() != nil {
			if !needsReconcile {
				return errors.New("reverted folder canonical inode unavailable")
			}
			if err := repository.VerifyRootedFolderIdentity(c.root, revert.AttemptedRelative, revert.Device, revert.Inode); err != nil {
				return err
			}
			if err := repository.MoveRootedFolderExpected(c.root, revert.AttemptedRelative, revert.CanonicalRelative, revert.Device, revert.Inode); err != nil {
				return err
			}
		}
		if testHookAfterConflictFolderMoveRevert != nil {
			testHookAfterConflictFolderMoveRevert()
		}
		if needsReconcile {
			options := reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteFolders: map[uuid.UUID]bool{}, AppliedRemoteFolderPaths: map[uuid.UUID]string{}, AppliedRemoteNotes: map[uuid.UUID][32]byte{}, AppliedRemoteNotePaths: map[uuid.UUID]string{}, TrustedRemoteFolderMoves: map[string]string{revert.AttemptedRelative: revert.CanonicalRelative}, VerifyTrustedRemoteFolders: verify}
			prefix := revert.AttemptedRelative + "/"
			for _, before := range snapshot.Objects {
				if before.RelativePath != revert.AttemptedRelative && !strings.HasPrefix(before.RelativePath, prefix) {
					continue
				}
				target := revert.CanonicalRelative + strings.TrimPrefix(before.RelativePath, revert.AttemptedRelative)
				if before.Type == localindex.ObjectFolder {
					options.AppliedRemoteFolders[before.ID] = true
					options.AppliedRemoteFolderPaths[before.ID] = target
				} else if len(before.ContentHash) == sha256.Size {
					var hash [sha256.Size]byte
					copy(hash[:], before.ContentHash)
					options.AppliedRemoteNotes[before.ID] = hash
					options.AppliedRemoteNotePaths[before.ID] = target
				}
			}
			if _, err := reconcile.Run(ctx, c.root, c.index, options); err != nil {
				return err
			}
			if err := verify(); err != nil {
				return err
			}
		}
		if testHookAfterConflictFolderMoveReconcile != nil {
			testHookAfterConflictFolderMoveReconcile()
		}
		if err := store.MarkConflictFolderMoveReverted(ctx, m.OperationID); err != nil {
			return err
		}
		revert.State = "moved"
	}
	if revert.State == "moved" {
		if err := verify(); err != nil {
			return err
		}
		if err := store.CompleteConflictFolderMoveRevert(ctx, m.OperationID); err != nil {
			return err
		}
		revert.State = "completed"
	}
	return verify()
}

func (c *LocalCore) restoreFolderNotEmptyConflict(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	m, canonical := conflict.Outbox.Mutation, conflict.Canonical
	if canonical.Revision != m.BaseRevision {
		return errors.New("folder-not-empty canonical revision mismatch")
	}
	restoration, err := store.ConflictFolderRestoration(ctx, m.OperationID)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	target, err := remoteNotePath(objects, canonical.ParentID, canonical.Name)
	if err != nil {
		return err
	}
	var parent *localindex.Object
	if canonical.ParentID != nil {
		value, ok := objects[*canonical.ParentID]
		if !ok || value.Type != localindex.ObjectFolder || value.IdentityState != localindex.IdentityKnown || value.FolderDevice == 0 || value.FolderInode == 0 {
			return errors.New("folder restoration parent identity unavailable")
		}
		parent = &value
	}
	if restoration == nil {
		if _, _, err := repository.RootedFolderIdentity(c.root, target); err == nil {
			return errors.New("folder restoration target already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		stage := ".remember/conflicts/restores/" + m.OperationID.String()
		if err := repository.RemoveRootedFolderPublicationStage(c.root, stage); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		device, inode, err := repository.CreateRootedFolderPublication(c.root, stage, nonce)
		if err != nil {
			return err
		}
		restoration = &clientsync.ConflictFolderRestoration{OperationID: m.OperationID, FolderID: m.ObjectID, TargetRelative: target, StageRelative: stage, Nonce: nonce, Device: device, Inode: inode, State: "prepared"}
		if err := store.PutConflictFolderRestoration(ctx, *restoration); err != nil {
			return err
		}
	}
	if restoration.FolderID != m.ObjectID || restoration.TargetRelative != target {
		return errors.New("folder restoration identity mismatch")
	}
	verify := func() error {
		if parent != nil {
			if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
				return err
			}
		}
		return repository.VerifyRootedFolderPublication(c.root, target, restoration.Nonce, restoration.Device, restoration.Inode)
	}
	if restoration.State == "prepared" {
		if verify() != nil {
			if err := repository.PublishRootedFolderPublication(c.root, restoration.StageRelative, target, restoration.Nonce, restoration.Device, restoration.Inode); err != nil {
				return err
			}
		}
		if testHookAfterConflictFolderPublication != nil {
			testHookAfterConflictFolderPublication()
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, TrustedRemoteFolders: map[string]uuid.UUID{target: m.ObjectID}, VerifyTrustedRemoteFolders: verify}); err != nil {
			return err
		}
		if err := verify(); err != nil {
			return err
		}
		if err := store.MarkConflictFolderRestorationPublished(ctx, m.OperationID); err != nil {
			return err
		}
		restoration.State = "published"
	}
	if restoration.State == "published" {
		if err := repository.CleanupRootedFolderPublication(c.root, target, restoration.Nonce, restoration.Device, restoration.Inode); err != nil {
			return err
		}
		if err := store.CompleteFolderNotEmptyResolution(ctx, m.OperationID); err != nil {
			return err
		}
		restoration.State = "completed"
	}
	if parent != nil {
		if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
			return err
		}
	}
	return repository.VerifyRootedFolderIdentity(c.root, target, restoration.Device, restoration.Inode)
}

func (c *LocalCore) ensureLocalConflictNamespace(ctx context.Context, store *clientsync.Store) error {
	folders := []struct {
		id     uuid.UUID
		target string
	}{
		{clientsync.ConflictRootID, clientsync.ConflictRootName},
		{clientsync.ConflictRecoveredID, clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName},
	}
	var rootBinding *clientsync.ConflictFolderPublication
	for _, folder := range folders {
		publication, err := store.ConflictFolderPublication(ctx, folder.id)
		if err != nil {
			return err
		}
		if publication == nil {
			snapshot, err := c.index.ReadSnapshot(ctx)
			if err != nil {
				return err
			}
			for _, object := range snapshot.Objects {
				if object.ID == folder.id && object.Type == localindex.ObjectFolder && object.RelativePath == folder.target && object.IdentityState == localindex.IdentityKnown {
					if err := repository.VerifyRootedFolderIdentity(c.root, folder.target, object.FolderDevice, object.FolderInode); err != nil {
						return err
					}
					publication = &clientsync.ConflictFolderPublication{FolderID: folder.id, TargetRelative: folder.target, Device: object.FolderDevice, Inode: object.FolderInode, State: "cleaned"}
					break
				}
			}
			if publication == nil {
				if _, _, err := repository.RootedFolderIdentity(c.root, folder.target); err == nil {
					return errors.New("unbound reserved conflict folder exists")
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				stage := ".remember/conflicts/folders/" + folder.id.String()
				if err := repository.RemoveRootedFolderPublicationStage(c.root, stage); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				var nonce [32]byte
				if _, err := rand.Read(nonce[:]); err != nil {
					return err
				}
				device, inode, err := repository.CreateRootedFolderPublication(c.root, stage, nonce)
				if err != nil {
					return err
				}
				publication = &clientsync.ConflictFolderPublication{FolderID: folder.id, TargetRelative: folder.target, StageRelative: stage, Nonce: nonce, Device: device, Inode: inode, State: "prepared"}
				if err := store.PutConflictFolderPublication(ctx, *publication); err != nil {
					return err
				}
			}
		}
		if publication.TargetRelative != folder.target || publication.FolderID != folder.id {
			return errors.New("reserved conflict folder publication mismatch")
		}
		if publication.State == "prepared" {
			if folder.id == clientsync.ConflictRecoveredID && testHookBeforeConflictRecoveredPublication != nil {
				testHookBeforeConflictRecoveredPublication()
			}
			if repository.VerifyRootedFolderPublication(c.root, folder.target, publication.Nonce, publication.Device, publication.Inode) != nil {
				if err := repository.PublishRootedFolderPublication(c.root, publication.StageRelative, folder.target, publication.Nonce, publication.Device, publication.Inode); err != nil {
					return err
				}
			}
			verify := func() error {
				if rootBinding != nil {
					if err := repository.VerifyRootedFolderIdentity(c.root, rootBinding.TargetRelative, rootBinding.Device, rootBinding.Inode); err != nil {
						return err
					}
				}
				return repository.VerifyRootedFolderPublication(c.root, folder.target, publication.Nonce, publication.Device, publication.Inode)
			}
			if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, TrustedRemoteFolders: map[string]uuid.UUID{folder.target: folder.id}, VerifyTrustedRemoteFolders: verify}); err != nil {
				return err
			}
			if err := verify(); err != nil {
				return err
			}
			if err := store.MarkConflictFolderPublication(ctx, folder.id, "published"); err != nil {
				return err
			}
			publication.State = "published"
		}
		if publication.State == "published" {
			if err := repository.CleanupRootedFolderPublication(c.root, folder.target, publication.Nonce, publication.Device, publication.Inode); err != nil {
				return err
			}
			if err := store.MarkConflictFolderPublication(ctx, folder.id, "cleaned"); err != nil {
				return err
			}
		}
		if err := repository.VerifyRootedFolderIdentity(c.root, folder.target, publication.Device, publication.Inode); err != nil {
			return err
		}
		if folder.id == clientsync.ConflictRootID {
			copyPublication := *publication
			rootBinding = &copyPublication
		} else if rootBinding == nil {
			return errors.New("reserved conflict root binding unavailable")
		} else if err := repository.VerifyRootedFolderIdentity(c.root, rootBinding.TargetRelative, rootBinding.Device, rootBinding.Inode); err != nil {
			return err
		}
	}
	return nil
}

func (c *LocalCore) stageNoteCanonicalAbsentConflict(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	op := conflict.Outbox.Mutation.OperationID
	materialization, err := store.ConflictMaterialization(ctx, op)
	if err != nil {
		return err
	}
	if materialization == nil {
		snapshot, err := c.index.ReadSnapshot(ctx)
		if err != nil {
			return err
		}
		var object localindex.Object
		found := false
		for _, candidate := range snapshot.Objects {
			if candidate.ID == conflict.Outbox.Mutation.ObjectID && candidate.Type == localindex.ObjectNote && candidate.IdentityState == localindex.IdentityKnown {
				object, found = candidate, true
				break
			}
		}
		if !found {
			return errors.New("path-colliding local note unavailable")
		}
		content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		sourceHash := sha256.Sum256(content)
		if err := clientsync.StageNote(c.root, object.RelativePath, sourceHash); err != nil {
			return err
		}
		conflictID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		copyBytes, err := frontmatter.MaterializeConflictCopy(content, object.ID, conflictID, frontmatter.ConflictOrigin{OriginalNoteID: object.ID, OperationID: op, Reason: conflict.Code, OriginalTarget: object.RelativePath})
		if err != nil {
			return err
		}
		if int64(len(copyBytes)) > clientsync.MaxBlobBytes {
			return errors.New("conflict copy exceeds sync blob limit")
		}
		materialization = &clientsync.ConflictMaterialization{OperationID: op, SourceObjectID: object.ID, ConflictNoteID: conflictID, OriginalRelative: object.RelativePath, TargetRelative: clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFileName(path.Base(object.RelativePath), op), SourceHash: sourceHash, MaterializedHash: sha256.Sum256(copyBytes), StagedRelative: ".remember/conflicts/materializations/" + op.String() + ".md", State: "prepared"}
		if err := store.PutConflictMaterialization(ctx, *materialization); err != nil {
			return err
		}
	}
	if materialization.State != "prepared" {
		return nil
	}
	source, err := clientsync.ReadStagedNote(c.root, materialization.SourceHash)
	if err != nil {
		return err
	}
	copyBytes, err := frontmatter.MaterializeConflictCopy(source, materialization.SourceObjectID, materialization.ConflictNoteID, frontmatter.ConflictOrigin{OriginalNoteID: materialization.SourceObjectID, OperationID: op, Reason: conflict.Code, OriginalTarget: materialization.OriginalRelative})
	if err != nil {
		return err
	}
	if sha256.Sum256(copyBytes) != materialization.MaterializedHash {
		return errors.New("path collision materialization changed")
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts/materializations", 0o700); err != nil {
		return err
	}
	if err := repository.CreateRootedPrivate(c.root, materialization.StagedRelative, copyBytes); err != nil {
		existing, readErr := repository.ReadRootedPrivate(c.root, materialization.StagedRelative, clientsync.MaxBlobBytes)
		if readErr != nil || !bytes.Equal(existing, copyBytes) {
			return err
		}
	}
	return store.MarkConflictCopyStaged(ctx, op)
}

func (c *LocalCore) stageNoteMovePathCollision(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	op := conflict.Outbox.Mutation.OperationID
	materialization, err := store.ConflictMaterialization(ctx, op)
	if err != nil {
		return err
	}
	if materialization == nil {
		m := conflict.Outbox.Mutation
		if conflict.Code == "base_revision_mismatch" && m.ParentID != nil {
			if err := c.validateDivergentNestedNoteMove(ctx, store, m); err != nil {
				return err
			}
		}
		snapshot, err := c.index.ReadSnapshot(ctx)
		if err != nil {
			return err
		}
		var object localindex.Object
		found := false
		for _, candidate := range snapshot.Objects {
			if candidate.ID == conflict.Outbox.Mutation.ObjectID && candidate.Type == localindex.ObjectNote && candidate.IdentityState == localindex.IdentityKnown {
				object, found = candidate, true
				break
			}
		}
		if !found {
			return errors.New("path-colliding moved note unavailable")
		}
		if conflict.Code == "base_revision_mismatch" && m.ParentID != nil {
			objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
			for _, candidate := range snapshot.Objects {
				objects[candidate.ID] = candidate
			}
			expected, expectedErr := remoteNotePath(objects, m.ParentID, m.Name)
			if expectedErr != nil {
				return expectedErr
			}
			if object.RelativePath != expected {
				return errors.New("divergent nested note move staging path mismatch")
			}
		}
		content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != object.ID {
			return errors.New("path-colliding moved note identity changed")
		}
		sourceHash := sha256.Sum256(content)
		if err := clientsync.StageNote(c.root, object.RelativePath, sourceHash); err != nil {
			return err
		}
		if conflict.Code == "base_revision_mismatch" && m.ParentID != nil {
			if err := c.validateDivergentNestedNoteMove(ctx, store, m); err != nil {
				return err
			}
		}
		conflictID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		copyBytes, err := frontmatter.MaterializeConflictCopy(content, object.ID, conflictID, frontmatter.ConflictOrigin{OriginalNoteID: object.ID, OperationID: op, Reason: conflict.Code, OriginalTarget: object.RelativePath, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: conflict.Canonical.Revision})
		if err != nil {
			return err
		}
		if int64(len(copyBytes)) > clientsync.MaxBlobBytes {
			return errors.New("conflict copy exceeds sync blob limit")
		}
		materialization = &clientsync.ConflictMaterialization{OperationID: op, SourceObjectID: object.ID, ConflictNoteID: conflictID, OriginalRelative: object.RelativePath, TargetRelative: clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFileName(path.Base(object.RelativePath), op), SourceHash: sourceHash, MaterializedHash: sha256.Sum256(copyBytes), StagedRelative: ".remember/conflicts/materializations/" + op.String() + ".md", State: "prepared"}
		if err := store.PutConflictMaterialization(ctx, *materialization); err != nil {
			return err
		}
	}
	if materialization.State != "prepared" {
		return nil
	}
	source, err := clientsync.ReadStagedNote(c.root, materialization.SourceHash)
	if err != nil {
		return err
	}
	copyBytes, err := frontmatter.MaterializeConflictCopy(source, materialization.SourceObjectID, materialization.ConflictNoteID, frontmatter.ConflictOrigin{OriginalNoteID: materialization.SourceObjectID, OperationID: op, Reason: conflict.Code, OriginalTarget: materialization.OriginalRelative, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: conflict.Canonical.Revision})
	if err != nil {
		return err
	}
	if sha256.Sum256(copyBytes) != materialization.MaterializedHash {
		return errors.New("move path collision materialization changed")
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts/materializations", 0o700); err != nil {
		return err
	}
	if err := repository.CreateRootedPrivate(c.root, materialization.StagedRelative, copyBytes); err != nil {
		existing, readErr := repository.ReadRootedPrivate(c.root, materialization.StagedRelative, clientsync.MaxBlobBytes)
		if readErr != nil || !bytes.Equal(existing, copyBytes) {
			return err
		}
	}
	return store.MarkConflictCopyStaged(ctx, op)
}

func (c *LocalCore) stageNoteUpdateConflict(ctx context.Context, store *clientsync.Store, conflict clientsync.ConflictItem) error {
	op := conflict.Outbox.Mutation.OperationID
	materialization, err := store.ConflictMaterialization(ctx, op)
	if err != nil {
		return err
	}
	if materialization == nil {
		snapshot, err := c.index.ReadSnapshot(ctx)
		if err != nil {
			return err
		}
		var object localindex.Object
		found := false
		for _, candidate := range snapshot.Objects {
			if candidate.ID == conflict.Outbox.Mutation.ObjectID && candidate.Type == localindex.ObjectNote && candidate.IdentityState == localindex.IdentityKnown {
				object = candidate
				found = true
				break
			}
		}
		if !found {
			return errors.New("conflicted local note is unavailable")
		}
		content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != object.ID {
			return errors.New("conflicted local note identity changed")
		}
		sourceHash := sha256.Sum256(content)
		if err := clientsync.StageNote(c.root, object.RelativePath, sourceHash); err != nil {
			return err
		}
		conflictID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		target := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFileName(path.Base(object.RelativePath), op)
		copyBytes, err := frontmatter.MaterializeConflictCopy(content, object.ID, conflictID, frontmatter.ConflictOrigin{OriginalNoteID: object.ID, OperationID: op, Reason: conflict.Code, OriginalTarget: object.RelativePath, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: conflict.Canonical.Revision})
		if err != nil {
			return err
		}
		if int64(len(copyBytes)) > clientsync.MaxBlobBytes {
			return errors.New("conflict copy exceeds sync blob limit")
		}
		materializedHash := sha256.Sum256(copyBytes)
		materialization = &clientsync.ConflictMaterialization{OperationID: op, SourceObjectID: object.ID, ConflictNoteID: conflictID, OriginalRelative: object.RelativePath, TargetRelative: target, SourceHash: sourceHash, MaterializedHash: materializedHash, StagedRelative: ".remember/conflicts/materializations/" + op.String() + ".md", State: "prepared"}
		if err := store.PutConflictMaterialization(ctx, *materialization); err != nil {
			return err
		}
	}
	if materialization.State != "prepared" {
		return nil
	}
	source, err := clientsync.ReadStagedNote(c.root, materialization.SourceHash)
	if err != nil {
		return err
	}
	copyBytes, err := frontmatter.MaterializeConflictCopy(source, materialization.SourceObjectID, materialization.ConflictNoteID, frontmatter.ConflictOrigin{OriginalNoteID: materialization.SourceObjectID, OperationID: materialization.OperationID, Reason: conflict.Code, OriginalTarget: materialization.OriginalRelative, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: conflict.Canonical.Revision})
	if err != nil {
		return err
	}
	if int64(len(copyBytes)) > clientsync.MaxBlobBytes {
		return errors.New("conflict copy exceeds sync blob limit")
	}
	if sha256.Sum256(copyBytes) != materialization.MaterializedHash {
		return errors.New("conflict copy staging hash mismatch")
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts/materializations", 0o700); err != nil {
		return err
	}
	if err := repository.CreateRootedPrivate(c.root, materialization.StagedRelative, copyBytes); err != nil {
		existing, readErr := repository.ReadRootedPrivate(c.root, materialization.StagedRelative, clientsync.MaxBlobBytes)
		if readErr != nil || !bytes.Equal(existing, copyBytes) {
			return err
		}
	}
	return store.MarkConflictCopyStaged(ctx, op)
}

func (c *LocalCore) stageNoteDeleteConflict(ctx context.Context, store *clientsync.Store, resolver clientsync.BlobResolver, conflict clientsync.ConflictItem) error {
	op := conflict.Outbox.Mutation.OperationID
	materialization, err := store.ConflictMaterialization(ctx, op)
	if err != nil {
		return err
	}
	canonical := conflict.Canonical
	if materialization == nil {
		conflictID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		rebaseID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		sourceHash := itemHash(canonical.BlobHash)
		source, err := resolver.ResolveBlob(ctx, sourceHash)
		if err != nil {
			return err
		}
		if int64(len(source)) > clientsync.MaxBlobBytes || sha256.Sum256(source) != sourceHash {
			return errors.New("canonical delete-conflict blob invalid")
		}
		inspection, err := frontmatter.Inspect(source)
		if err != nil || inspection.NoteID != conflict.Outbox.Mutation.ObjectID {
			return errors.New("canonical delete-conflict note identity mismatch")
		}
		original := canonical.Name
		target := clientsync.ConflictRootName + "/" + clientsync.ConflictRecoveredName + "/" + clientsync.ConflictFileName(canonical.Name, op)
		copyBytes, err := frontmatter.MaterializeConflictCopy(source, conflict.Outbox.Mutation.ObjectID, conflictID, frontmatter.ConflictOrigin{OriginalNoteID: conflict.Outbox.Mutation.ObjectID, OperationID: op, Reason: conflict.Code, OriginalTarget: original, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: canonical.Revision})
		if err != nil {
			return err
		}
		if int64(len(copyBytes)) > clientsync.MaxBlobBytes {
			return errors.New("conflict copy exceeds sync blob limit")
		}
		materialization = &clientsync.ConflictMaterialization{OperationID: op, SourceObjectID: conflict.Outbox.Mutation.ObjectID, ConflictNoteID: conflictID, OriginalRelative: original, TargetRelative: target, SourceHash: sourceHash, MaterializedHash: sha256.Sum256(copyBytes), StagedRelative: ".remember/conflicts/materializations/" + op.String() + ".md", State: "prepared", RebasedOperationID: &rebaseID}
		if err := store.PutConflictMaterialization(ctx, *materialization); err != nil {
			return err
		}
	}
	if materialization.State != "prepared" {
		return nil
	}
	sourceHash := materialization.SourceHash
	source, err := resolver.ResolveBlob(ctx, sourceHash)
	if err != nil {
		return err
	}
	copyBytes, err := frontmatter.MaterializeConflictCopy(source, materialization.SourceObjectID, materialization.ConflictNoteID, frontmatter.ConflictOrigin{OriginalNoteID: materialization.SourceObjectID, OperationID: op, Reason: conflict.Code, OriginalTarget: materialization.OriginalRelative, BaseRevision: conflict.Outbox.Mutation.BaseRevision, CanonicalRevision: canonical.Revision})
	if err != nil {
		return err
	}
	if sha256.Sum256(copyBytes) != materialization.MaterializedHash {
		return errors.New("delete conflict materialization changed")
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/conflicts/materializations", 0o700); err != nil {
		return err
	}
	if err := repository.CreateRootedPrivate(c.root, materialization.StagedRelative, copyBytes); err != nil {
		existing, readErr := repository.ReadRootedPrivate(c.root, materialization.StagedRelative, clientsync.MaxBlobBytes)
		if readErr != nil || !bytes.Equal(existing, copyBytes) {
			return err
		}
	}
	return store.MarkConflictCopyStaged(ctx, op)
}

func (c *LocalCore) ensurePathCollisionEvacuated(ctx context.Context, item clientsync.ConflictMaterialization) error {
	if err := repository.EnsureRootedDirectory(c.root, ".remember/trash", 0o700); err != nil {
		return err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/trash/conflicts", 0o700); err != nil {
		return err
	}
	trash := ".remember/trash/conflicts/" + item.OperationID.String() + ".md"
	expected, err := clientsync.ReadStagedNote(c.root, item.SourceHash)
	if err != nil {
		return err
	}
	finish := func() error {
		_, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteDeletes: map[uuid.UUID]bool{item.SourceObjectID: true}})
		return err
	}
	source, err := repository.ReadRooted(c.root, item.OriginalRelative, clientsync.MaxBlobBytes)
	if err == nil && sha256.Sum256(source) != item.SourceHash {
		existing, trashErr := repository.ReadRooted(c.root, trash, clientsync.MaxBlobBytes)
		if trashErr == nil && sha256.Sum256(existing) == item.SourceHash {
			return finish()
		}
		return errors.New("path-collision source changed")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	moveErr := repository.MoveRootedExpected(c.root, item.OriginalRelative, trash, expected)
	if moveErr == nil {
		if testHookAfterConflictEvacuation != nil {
			testHookAfterConflictEvacuation()
		}
		return finish()
	}
	if !errors.Is(moveErr, os.ErrNotExist) && !errors.Is(moveErr, os.ErrExist) {
		return moveErr
	}
	evacuated, err := repository.ReadRooted(c.root, trash, clientsync.MaxBlobBytes)
	if err != nil || sha256.Sum256(evacuated) != item.SourceHash {
		return errors.New("path-collision evacuation unavailable")
	}
	return finish()
}

func (c *LocalCore) verifyPathCollisionApplied(ctx context.Context, store *clientsync.Store, item clientsync.ConflictMaterialization) error {
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, object := range snapshot.Objects {
		if object.RelativePath != item.OriginalRelative {
			continue
		}
		if object.ID == item.SourceObjectID || object.Type != localindex.ObjectNote || object.IdentityState == localindex.IdentityPending {
			return errors.New("remote path-collision winner is not applied")
		}
		matched, err := store.BaselineMatchesAppliedNote(ctx, object.ID, object.ParentID, path.Base(object.RelativePath), object.ContentHash)
		if err != nil {
			return err
		}
		if !matched {
			return errors.New("path-collision winner lacks authenticated apply baseline")
		}
		content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != object.ID {
			return errors.New("remote path-collision winner identity mismatch")
		}
		return nil
	}
	return errors.New("remote path-collision winner is absent")
}

func (c *LocalCore) verifyOrphanConflictEvacuated(ctx context.Context, item clientsync.ConflictMaterialization) error {
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, object := range snapshot.Objects {
		if object.ID == item.SourceObjectID {
			return errors.New("object-missing conflict source remains indexed")
		}
	}
	trash := ".remember/trash/conflicts/" + item.OperationID.String() + ".md"
	content, err := repository.ReadRooted(c.root, trash, clientsync.MaxBlobBytes)
	if err != nil || sha256.Sum256(content) != item.SourceHash {
		return errors.New("object-missing conflict evacuation unavailable")
	}
	return nil
}

func (c *LocalCore) ensureMoveCollisionCanonical(ctx context.Context, store *clientsync.Store, item clientsync.ConflictMaterialization, canonical clientsync.CanonicalState, resolver clientsync.BlobResolver) (bool, error) {
	if resolver == nil {
		return false, nil
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return false, err
	}
	directory := ""
	var parent *localindex.Object
	if canonical.ParentID != nil {
		for i := range snapshot.Objects {
			object := &snapshot.Objects[i]
			if object.ID == *canonical.ParentID && object.Type == localindex.ObjectFolder && object.IdentityState == localindex.IdentityKnown && object.FolderDevice > 0 && object.FolderInode > 0 {
				directory = object.RelativePath
				parent = object
				break
			}
		}
		if parent == nil {
			return false, nil
		}
		unresolved, err := store.HasUnresolvedLocalIntent(ctx, *canonical.ParentID)
		if err != nil {
			return false, err
		}
		if unresolved {
			return false, errors.New("move collision canonical parent has unresolved local intent")
		}
		if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
			return false, err
		}
	}
	verifyParent := func() error {
		if parent == nil {
			return nil
		}
		if err := repository.VerifyRootedFolderIdentity(c.root, parent.RelativePath, parent.FolderDevice, parent.FolderInode); err != nil {
			return err
		}
		unresolved, err := store.HasUnresolvedLocalIntent(ctx, *canonical.ParentID)
		if err != nil {
			return err
		}
		if unresolved {
			return errors.New("move collision canonical parent changed")
		}
		return nil
	}
	target := canonical.Name
	if directory != "" {
		target = path.Join(directory, canonical.Name)
	}
	if target == item.OriginalRelative {
		return false, errors.New("move collision canonical path equals attempted path")
	}
	content, readErr := repository.ReadRooted(c.root, target, clientsync.MaxBlobBytes)
	hash := itemHash(canonical.BlobHash)
	if readErr == nil {
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != item.SourceObjectID || sha256.Sum256(content) != hash {
			return false, errors.New("move collision canonical target occupied")
		}
	} else {
		if !errors.Is(readErr, os.ErrNotExist) {
			return false, readErr
		}
		content, err = resolver.ResolveBlob(ctx, hash)
		if err != nil {
			return false, err
		}
		if int64(len(content)) > clientsync.MaxBlobBytes || sha256.Sum256(content) != hash {
			return false, errors.New("move collision canonical blob invalid")
		}
		if parent != nil {
			if err := repository.CreateRootedInFolderExpected(c.root, parent.RelativePath, canonical.Name, parent.FolderDevice, parent.FolderInode, content, validateAppliedNote(item.SourceObjectID)); err != nil {
				return false, err
			}
		} else if err := repository.CreateRooted(c.root, target, content, validateAppliedNote(item.SourceObjectID)); err != nil {
			return false, err
		}
	}
	if err := verifyParent(); err != nil {
		return false, err
	}
	if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: map[uuid.UUID][32]byte{item.SourceObjectID: hash}}); err != nil {
		return false, err
	}
	return true, nil
}

func (c *LocalCore) publishStagedConflicts(ctx context.Context, store *clientsync.Store, resolver clientsync.BlobResolver) error {
	items, err := store.StagedConflictMaterializations(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		kind, err := store.ConflictMutationKind(ctx, item.OperationID)
		if err != nil {
			return err
		}
		code, err := store.ConflictCode(ctx, item.OperationID)
		if err != nil {
			return err
		}
		if kind == clientsync.Create || kind == clientsync.Move || kind == clientsync.Update && code == "object_missing" {
			if err := c.ensurePathCollisionEvacuated(ctx, item); err != nil {
				return err
			}
		}
		if kind == clientsync.Move && (code == "path_collision" || code == "parent_unavailable" || code == "base_revision_mismatch") {
			canonical, err := store.CanonicalConflictState(ctx, item.OperationID)
			if err != nil {
				return err
			}
			if canonical == nil {
				return errors.New("move collision canonical state unavailable")
			}
			ready, err := c.ensureMoveCollisionCanonical(ctx, store, item, *canonical, resolver)
			if err != nil {
				return err
			}
			if !ready {
				return nil
			}
		}
	}
	_, _, durable, err := store.Projection(ctx, clientsync.ConflictRecoveredID)
	if err != nil {
		return err
	}
	if !durable {
		return nil
	}
	for _, item := range items {
		kind, err := store.ConflictMutationKind(ctx, item.OperationID)
		if err != nil {
			return err
		}
		canonical, err := store.CanonicalConflictState(ctx, item.OperationID)
		if err != nil {
			return err
		}
		code, err := store.ConflictCode(ctx, item.OperationID)
		if err != nil {
			return err
		}
		orphan := canonical == nil && (((kind == clientsync.Update || kind == clientsync.Move) && code == "object_missing") || (kind == clientsync.Create && code == "parent_unavailable"))
		if canonical == nil && kind != clientsync.Create && !orphan {
			return errors.New("canonical conflict state unavailable")
		}
		localDelete := kind == clientsync.Delete
		if orphan {
			if err := c.verifyOrphanConflictEvacuated(ctx, item); err != nil {
				return err
			}
		} else if kind == clientsync.Create {
			if err := c.verifyPathCollisionApplied(ctx, store, item); err != nil {
				return err
			}
		} else {
			revision, _, sourceDurable, err := store.Projection(ctx, item.SourceObjectID)
			if err != nil {
				return err
			}
			if !sourceDurable || revision < canonical.Revision {
				return nil
			}
			if revision != canonical.Revision {
				return errors.New("conflicted note has newer local intent")
			}
			if err := c.verifyCanonicalConflictApplied(ctx, item, *canonical, localDelete); err != nil {
				return err
			}
		}
		content, err := repository.ReadRootedPrivate(c.root, item.StagedRelative, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		if sha256.Sum256(content) != item.MaterializedHash {
			return errors.New("staged conflict copy changed")
		}
		if item.State == "copy_staged" {
			if err := repository.CreateRooted(c.root, item.TargetRelative, content, validateAppliedNote(item.ConflictNoteID)); err != nil {
				existing, readErr := repository.ReadRooted(c.root, item.TargetRelative, clientsync.MaxBlobBytes)
				if readErr != nil || !bytes.Equal(existing, content) {
					return err
				}
			}
			if err := store.MarkConflictCopyPublished(ctx, item.OperationID); err != nil {
				return err
			}
			item.State = "copy_published"
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode}); err != nil {
			return err
		}
		if orphan {
			if err := c.verifyOrphanConflictEvacuated(ctx, item); err != nil {
				return err
			}
		} else if kind == clientsync.Create {
			if err := c.verifyPathCollisionApplied(ctx, store, item); err != nil {
				return err
			}
		} else if err := c.verifyCanonicalConflictApplied(ctx, item, *canonical, localDelete); err != nil {
			return err
		}
		if testHookBeforeConflictCompletion != nil {
			testHookBeforeConflictCompletion()
		}
		if localDelete {
			if err := store.CompleteConflictMaterializationAndRebaseDelete(ctx, item, *canonical); err != nil {
				return err
			}
		} else if err := store.CompleteConflictMaterialization(ctx, item.OperationID); err != nil {
			return err
		}
	}
	return c.cleanupCompletedConflictStages(ctx, store)
}

func (c *LocalCore) cleanupCompletedOutboxBlobs(ctx context.Context, store *clientsync.Store) error {
	for {
		hashes, err := store.PendingOutboxBlobCleanups(ctx, 100)
		if err != nil {
			return err
		}
		if len(hashes) == 0 {
			return nil
		}
		for _, item := range hashes {
			relative := ".remember/sync/outbox/" + hex.EncodeToString(item.Hash[:])
			if err := repository.RemoveRootedOutboxBlobExpected(c.root, relative, item.Hash, item.ThroughSequence); err != nil {
				return err
			}
			if err := store.MarkOutboxBlobCleaned(ctx, item); err != nil {
				return err
			}
		}
		if len(hashes) < 100 {
			return nil
		}
	}
}

func (c *LocalCore) cleanupCompletedConflictStages(ctx context.Context, store *clientsync.Store) error {
	items, err := store.CompletedConflictCleanups(ctx)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		visible := false
		for _, object := range snapshot.Objects {
			if object.ID != item.ConflictNoteID {
				continue
			}
			if object.Type != localindex.ObjectNote || object.IdentityState == localindex.IdentityPending || !bytes.Equal(object.ContentHash, item.MaterializedHash[:]) {
				return errors.New("materialized conflict copy is not durably indexed")
			}
			content, readErr := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
			if readErr != nil || sha256.Sum256(content) != item.MaterializedHash {
				return errors.New("materialized conflict copy is unavailable")
			}
			inspection, inspectErr := frontmatter.Inspect(content)
			if inspectErr != nil || inspection.NoteID != item.ConflictNoteID {
				return errors.New("materialized conflict copy identity mismatch")
			}
			visible = true
			break
		}
		if !visible {
			return errors.New("materialized conflict copy is absent")
		}
		kind, err := store.ConflictMutationKind(ctx, item.OperationID)
		if err != nil {
			return err
		}
		code, err := store.ConflictCode(ctx, item.OperationID)
		if err != nil {
			return err
		}
		if ((kind == clientsync.Create || kind == clientsync.Move) && (code == "path_collision" || code == "parent_unavailable" || kind == clientsync.Move && code == "base_revision_mismatch")) || ((kind == clientsync.Update || kind == clientsync.Move) && code == "object_missing") || (kind == clientsync.Move && code == "object_deleted") {
			evacuated := ".remember/trash/conflicts/" + item.OperationID.String() + ".md"
			if err := repository.RemoveRootedConflictEvacuationExpected(c.root, evacuated, item.SourceHash); err != nil {
				return err
			}
		}
		if err := repository.RemoveRootedConflictStageExpected(c.root, item.StagedRelative, item.MaterializedHash); err != nil {
			return err
		}
		if err := store.MarkConflictMaterializationCleaned(ctx, item.OperationID); err != nil {
			return err
		}
	}
	return nil
}

func (c *LocalCore) verifyCanonicalConflictApplied(ctx context.Context, item clientsync.ConflictMaterialization, canonical clientsync.CanonicalState, localDelete bool) error {
	if canonical.ObjectType != clientsync.Note || len(canonical.BlobHash) != sha256.Size {
		return errors.New("unsupported canonical conflict state")
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	if canonical.Deleted || localDelete {
		for _, object := range snapshot.Objects {
			if object.ID == item.SourceObjectID {
				return errors.New("canonically deleted conflicted note remains indexed")
			}
			if object.RelativePath == item.OriginalRelative {
				return errors.New("canonically deleted note path is occupied")
			}
		}
		if _, err := repository.ReadRooted(c.root, item.OriginalRelative, 1); !errors.Is(err, os.ErrNotExist) {
			return errors.New("canonically deleted conflicted note remains visible")
		}
		return nil
	}
	for _, object := range snapshot.Objects {
		if object.ID != item.SourceObjectID {
			continue
		}
		if object.Type != localindex.ObjectNote || object.IdentityState != localindex.IdentityKnown || path.Base(object.RelativePath) != canonical.Name || !bytes.Equal(object.ContentHash, canonical.BlobHash) {
			return errors.New("canonical conflicted note is not applied")
		}
		if canonical.ParentID == nil && object.ParentID != uuid.Nil || canonical.ParentID != nil && object.ParentID != *canonical.ParentID {
			return errors.New("canonical conflicted note parent mismatch")
		}
		content, err := repository.ReadRooted(c.root, object.RelativePath, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		if sha256.Sum256(content) != itemHash(canonical.BlobHash) {
			return errors.New("canonical conflicted note bytes changed")
		}
		return nil
	}
	return errors.New("canonical conflicted note is absent")
}

func itemHash(value []byte) (result [32]byte) { copy(result[:], value); return }
