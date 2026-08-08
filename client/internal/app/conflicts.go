package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

var testHookBeforeConflictRecoveredPublication func()
var testHookBeforeConflictCompletion func()
var testHookAfterConflictEvacuation func()
var testHookAfterConflictFolderPublication func()

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
		if m.ObjectType == clientsync.Folder && m.Kind == clientsync.Delete && conflict.Code == "folder_not_empty" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Folder && !conflict.Canonical.Deleted {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := c.restoreFolderNotEmptyConflict(ctx, store, conflict); err != nil {
				return err
			}
			continue
		}
		if m.Kind == clientsync.Delete && conflict.Code == "object_missing" && conflict.Canonical == nil {
			if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
				return err
			}
			if err := store.ResolveMissingDelete(ctx, m.OperationID); err != nil {
				return err
			}
			continue
		}
		canonicalAbsent := conflict.Canonical == nil && ((m.Kind == clientsync.Create && conflict.Code == "path_collision") || ((m.Kind == clientsync.Update || m.Kind == clientsync.Move) && conflict.Code == "object_missing"))
		moveCollision := m.Kind == clientsync.Move && conflict.Code == "path_collision" && conflict.Canonical != nil && conflict.Canonical.ObjectType == clientsync.Note && !conflict.Canonical.Deleted && len(conflict.Canonical.BlobHash) == sha256.Size
		if m.ObjectType == clientsync.Note && (canonicalAbsent || moveCollision) {
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
		localUpdate := m.Kind == clientsync.Update && ((conflict.Code == "base_revision_mismatch" && !conflict.Canonical.Deleted) || (conflict.Code == "object_deleted" && conflict.Canonical.Deleted))
		localDelete := m.Kind == clientsync.Delete && conflict.Code == "base_revision_mismatch" && !conflict.Canonical.Deleted
		if !localUpdate && !localDelete {
			continue
		}
		if localDelete && resolver == nil {
			continue
		}
		if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
			return err
		}
		if localUpdate {
			if err := c.stageNoteUpdateConflict(ctx, store, conflict); err != nil {
				return err
			}
		} else if err := c.stageNoteDeleteConflict(ctx, store, resolver, conflict); err != nil {
			return err
		}
	}
	return nil
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
	if canonical.ParentID != nil {
		found := false
		for _, object := range snapshot.Objects {
			if object.ID == *canonical.ParentID && object.Type == localindex.ObjectFolder && object.IdentityState == localindex.IdentityKnown {
				directory = object.RelativePath
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
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
		if err := repository.CreateRooted(c.root, target, content, validateAppliedNote(item.SourceObjectID)); err != nil {
			return false, err
		}
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
		if kind == clientsync.Move && code == "path_collision" {
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
		orphan := canonical == nil && (kind == clientsync.Update || kind == clientsync.Move) && code == "object_missing"
		if canonical == nil && kind != clientsync.Create && !orphan {
			return errors.New("canonical conflict state unavailable")
		}
		localDelete := kind == clientsync.Delete
		if kind == clientsync.Create {
			if err := c.verifyPathCollisionApplied(ctx, store, item); err != nil {
				return err
			}
		} else if orphan {
			if err := c.verifyOrphanConflictEvacuated(ctx, item); err != nil {
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
		if kind == clientsync.Create {
			if err := c.verifyPathCollisionApplied(ctx, store, item); err != nil {
				return err
			}
		} else if orphan {
			if err := c.verifyOrphanConflictEvacuated(ctx, item); err != nil {
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
		if (kind == clientsync.Create || kind == clientsync.Move) && code == "path_collision" || (kind == clientsync.Update || kind == clientsync.Move) && code == "object_missing" {
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
