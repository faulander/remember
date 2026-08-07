package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

func (c *LocalCore) stageSupportedConflicts(ctx context.Context, store *clientsync.Store) error {
	conflicts, err := store.ListConflicts(ctx, 100)
	if err != nil {
		return err
	}
	for _, conflict := range conflicts {
		m := conflict.Outbox.Mutation
		if conflict.Code != "base_revision_mismatch" || m.ObjectType != clientsync.Note || m.Kind != clientsync.Update || conflict.Canonical == nil || conflict.Canonical.ObjectType != clientsync.Note || conflict.Canonical.Deleted || len(conflict.Canonical.BlobHash) != sha256.Size || conflict.Canonical.Revision <= m.BaseRevision {
			continue
		}
		if err := c.ensureLocalConflictNamespace(ctx, store); err != nil {
			return err
		}
		if err := c.stageNoteUpdateConflict(ctx, store, conflict); err != nil {
			return err
		}
	}
	return nil
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

func (c *LocalCore) publishStagedConflicts(ctx context.Context, store *clientsync.Store) error {
	_, _, durable, err := store.Projection(ctx, clientsync.ConflictRecoveredID)
	if err != nil {
		return err
	}
	if !durable {
		return nil
	}
	items, err := store.StagedConflictMaterializations(ctx)
	if err != nil {
		return err
	}
	for _, item := range items {
		canonical, err := store.CanonicalConflictState(ctx, item.OperationID)
		if err != nil {
			return err
		}
		if canonical == nil {
			return errors.New("canonical conflict state unavailable")
		}
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
		if err := c.verifyCanonicalConflictApplied(ctx, item, *canonical); err != nil {
			return err
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
		if err := c.verifyCanonicalConflictApplied(ctx, item, *canonical); err != nil {
			return err
		}
		if err := store.CompleteConflictMaterialization(ctx, item.OperationID); err != nil {
			return err
		}
	}
	return nil
}

func (c *LocalCore) verifyCanonicalConflictApplied(ctx context.Context, item clientsync.ConflictMaterialization, canonical clientsync.CanonicalState) error {
	if canonical.ObjectType != clientsync.Note || canonical.Deleted || len(canonical.BlobHash) != sha256.Size {
		return errors.New("unsupported canonical conflict state")
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
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
