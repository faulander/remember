package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

var ErrUnsupportedApplyPlan = errors.New("apply plan contains unsupported remote changes")

// Test-only race hook after durable publication and before reconciliation.
var testHookAfterApplyPublication func()
var testHookAfterNoteApplyReconcile func()
var testHookBeforeFolderPublication func()
var testHookAfterFolderReconcile func()
var testHookAfterFolderMutationPublication func() error

type preparedNoteStep struct {
	index             int
	change            clientsync.Change
	relative, source  string
	trash             string
	expected, content []byte
	exists, deleted   bool
	locallyApplied    bool
	conflictDeferred  bool
	folderPublication *clientsync.FolderPublication
	folderMutation    bool
	folderDevice      uint64
	folderInode       uint64
}

// ExecuteActiveApplyPlan resumes the one durable remote ApplyPlan. This slice
// supports note CRUD and inode-bound folder Create, Move and Delete. It validates
// the entire pending plan before publishing canonical filesystem changes.
func (c *LocalCore) ExecuteActiveApplyPlan(ctx context.Context, resolver clientsync.BlobResolver) error {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.lifecycleMu.Lock()
	closed := c.closed
	c.lifecycleMu.Unlock()
	if closed {
		return ErrCoreClosed
	}
	return c.executeActiveApplyPlanLocked(ctx, resolver)
}

func (c *LocalCore) executeActiveApplyPlanLocked(ctx context.Context, resolver clientsync.BlobResolver) error {
	if resolver == nil {
		return errors.New("nil blob resolver")
	}
	if err := repository.EnsurePrivateStagingSupported(); err != nil {
		return fmt.Errorf("remote apply unavailable: %w", err)
	}
	store, err := clientsync.NewStore(c.index)
	if err != nil {
		return err
	}
	if err := c.cleanupCompletedFolderPublications(ctx, store); err != nil {
		return err
	}
	plan, err := store.ActiveApplyPlan(ctx)
	if err != nil || plan == nil {
		return err
	}
	steps, err := c.preflightNotePlan(ctx, plan, resolver)
	if err != nil {
		return err
	}
	if err := store.BeginApplyPlan(ctx, plan.ID); err != nil {
		return err
	}
	appliedFolders := make(map[uuid.UUID]bool)
	appliedFolderPaths := make(map[uuid.UUID]string)
	for _, step := range steps {
		if step.folderPublication != nil {
			if err := c.applyFolderCreateStep(ctx, store, plan.ID, step); err != nil {
				return err
			}
			appliedFolders[step.change.ObjectID] = true
			appliedFolderPaths[step.change.ObjectID] = step.relative
			continue
		}
		if step.folderMutation {
			if err := c.applyFolderMutationStep(ctx, store, plan.ID, step); err != nil {
				return err
			}
			if step.deleted {
				delete(appliedFolders, step.change.ObjectID)
				delete(appliedFolderPaths, step.change.ObjectID)
			} else {
				oldPrefix := step.source + "/"
				for id, current := range appliedFolderPaths {
					if current == step.source || strings.HasPrefix(current, oldPrefix) {
						appliedFolderPaths[id] = step.relative + strings.TrimPrefix(current, step.source)
					}
				}
				appliedFolders[step.change.ObjectID] = true
				appliedFolderPaths[step.change.ObjectID] = step.relative
			}
			continue
		}
		if step.change.State == "applied" {
			continue
		}
		if step.conflictDeferred {
			if err := store.MarkApplyStepApplied(ctx, plan.ID, step.index); err != nil {
				return err
			}
			continue
		}
		if err := c.publishNoteApplyStep(step); err != nil {
			return err
		}
		if testHookAfterApplyPublication != nil {
			testHookAfterApplyPublication()
		}
		if !step.deleted && (step.change.Mutation == clientsync.Create || step.change.Mutation == clientsync.Move) {
			if err := rejectPortableSiblingCollisionExcept(c.root, step.relative, step.source); err != nil {
				return err
			}
		}
		options := reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteFolders: appliedFolders, AppliedRemoteFolderPaths: appliedFolderPaths}
		if step.deleted {
			options.AppliedRemoteDeletes = map[uuid.UUID]bool{step.change.ObjectID: true}
		} else {
			options.AppliedRemoteNotes = map[uuid.UUID][32]byte{step.change.ObjectID: sha256.Sum256(step.content)}
			options.AppliedRemoteNotePaths = map[uuid.UUID]string{step.change.ObjectID: step.relative}
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, options); err != nil {
			return err
		}
		if err := c.verifyNoteApplyStep(step); err != nil {
			return err
		}
		if testHookAfterNoteApplyReconcile != nil {
			testHookAfterNoteApplyReconcile()
		}
		if err := store.MarkApplyStepApplied(ctx, plan.ID, step.index); err != nil {
			return err
		}
	}
	if err := store.CompleteApplyPlan(ctx, plan.ID); err != nil {
		return err
	}
	return c.cleanupCompletedFolderPublications(ctx, store)
}

func (c *LocalCore) preflightNotePlan(ctx context.Context, plan *clientsync.ApplyPlan, resolver clientsync.BlobResolver) ([]preparedNoteStep, error) {
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	store, err := clientsync.NewStore(c.index)
	if err != nil {
		return nil, err
	}
	objects := make(map[uuid.UUID]localindex.Object, len(snapshot.Objects))
	for _, object := range snapshot.Objects {
		objects[object.ID] = object
	}
	// Reject the whole plan and local-intent conflicts before resolving blobs or
	// touching the filesystem. This also protects already-journaled steps when
	// an offline edit happened before resume.
	checkedIntent := make(map[uuid.UUID]bool)
	plannedFolders := make(map[uuid.UUID]bool)
	for _, step := range plan.Steps {
		if step.ObjectType == clientsync.Folder && step.Mutation == clientsync.Create {
			plannedFolders[step.ObjectID] = true
		}
		if step.ObjectType == clientsync.Folder {
			validMutation := step.Mutation == clientsync.Create || step.Mutation == clientsync.Move || step.Mutation == clientsync.Delete
			validRevision := step.Mutation == clientsync.Create && step.Revision == 1 || step.Mutation != clientsync.Create && step.Revision >= 2
			if !validMutation || !validRevision || step.Deleted != (step.Mutation == clientsync.Delete) || len(step.BlobHash) != 0 || step.Name == "" || naming.ValidateComponent(step.Name) != nil {
				return nil, ErrUnsupportedApplyPlan
			}
		} else if step.ObjectType != clientsync.Note || (step.Mutation != clientsync.Create && step.Mutation != clientsync.Update && step.Mutation != clientsync.Move && step.Mutation != clientsync.Delete) || step.Deleted != (step.Mutation == clientsync.Delete) || len(step.BlobHash) != sha256.Size {
			return nil, ErrUnsupportedApplyPlan
		}
		if step.ObjectType == clientsync.Note && !strings.EqualFold(path.Ext(step.Name), ".md") {
			return nil, errors.New("remote note name must use .md extension")
		}
		if step.Mutation == clientsync.Create {
			if step.Revision != 1 || step.Name == "" || naming.ValidateComponent(step.Name) != nil {
				return nil, errors.New("invalid remote note create")
			}
		} else if step.Revision < 2 || step.Name == "" || naming.ValidateComponent(step.Name) != nil {
			return nil, errors.New("invalid remote note state")
		}
		if !checkedIntent[step.ObjectID] {
			unresolved, err := store.HasUnresolvedLocalIntent(ctx, step.ObjectID)
			if err != nil {
				return nil, err
			}
			if unresolved {
				return nil, errors.New("remote change conflicts with unresolved local intent")
			}
			checkedIntent[step.ObjectID] = true
		}
	}
	virtual := make(map[uuid.UUID]preparedNoteStep)
	pathOwners := make(map[string]uuid.UUID)
	for id, object := range objects {
		pathOwners[portablePathKey(object.RelativePath)] = id
	}
	vacatedPaths := make(map[string]bool)
	lastStep := make(map[uuid.UUID]int)
	for index, change := range plan.Steps {
		lastStep[change.ObjectID] = index
	}
	prepared := make([]preparedNoteStep, 0, len(plan.Steps))
	for index, change := range plan.Steps {
		if change.ObjectType == clientsync.Folder {
			step, err := c.preflightFolderStep(ctx, store, plan.ID, index, change, objects, pathOwners)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, step)
			if change.Mutation == clientsync.Move {
				for id, prior := range virtual {
					object, exists := objects[id]
					if exists && object.Type == localindex.ObjectNote {
						prior.relative = object.RelativePath
						virtual[id] = prior
					}
				}
			}
			continue
		}
		var hash [sha256.Size]byte
		copy(hash[:], change.BlobHash)
		blob, err := resolver.ResolveBlob(ctx, hash)
		if err != nil {
			return nil, fmt.Errorf("resolve remote blob: %w", err)
		}
		if int64(len(blob)) > clientsync.MaxBlobBytes || sha256.Sum256(blob) != hash {
			return nil, errors.New("remote blob failed size or hash validation")
		}
		inspection, err := frontmatter.Inspect(blob)
		if err != nil || !inspection.HasRemember || inspection.NoteID != change.ObjectID {
			return nil, errors.New("remote note identity mismatch")
		}
		if err := validateAppliedNote(change.ObjectID)(blob); err != nil {
			return nil, err
		}
		step := preparedNoteStep{index: index, change: change, content: blob, deleted: change.Deleted}
		target, err := remoteNotePath(objects, change.ParentID, change.Name)
		if err != nil {
			return nil, err
		}
		step.relative = target
		deferredConflict, deferredErr := stagedConflictDeferredChange(ctx, store, change)
		if deferredErr != nil {
			return nil, deferredErr
		}
		if deferredConflict != nil {
			object, exists := objects[change.ObjectID]
			if !exists && (change.State == "applied" || deferredConflict.RebasedOperationID != nil) {
				step.conflictDeferred = true
				prepared = append(prepared, step)
				continue
			}
			if !exists || object.Type != localindex.ObjectNote {
				return nil, errors.New("deferred delete conflict object is unavailable")
			}
			step.source, step.expected, step.conflictDeferred = object.RelativePath, append([]byte(nil), object.ContentHash...), true
			object.RelativePath, object.CollisionPath, object.ContentHash = target, portablePathKey(target), append([]byte(nil), change.BlobHash...)
			if change.ParentID == nil {
				object.ParentID = uuid.Nil
			} else {
				object.ParentID = *change.ParentID
			}
			objects[change.ObjectID] = object
			virtual[change.ObjectID] = step
			prepared = append(prepared, step)
			continue
		}
		if change.Mutation == clientsync.Delete {
			step.trash = ".remember/trash/" + change.ObjectID.String() + "-" + change.OperationID.String() + ".md"
		}
		var exactDeleteConflict *clientsync.ConflictMaterialization
		evacuatedDeleteConflict := false
		if change.Mutation == clientsync.Delete {
			exactDeleteConflict, err = stagedRemoteDeleteConflict(ctx, store, change)
			if err != nil {
				return nil, err
			}
			if exactDeleteConflict != nil {
				kind, kindErr := store.ConflictMutationKind(ctx, exactDeleteConflict.OperationID)
				if kindErr != nil {
					return nil, kindErr
				}
				evacuatedDeleteConflict = kind == clientsync.Move
			}
		}
		prior, hadPrior := virtual[change.ObjectID]
		if change.State == "applied" && lastStep[change.ObjectID] > index {
			step.source, step.expected, step.exists = target, blob, true
		} else if hadPrior && exactDeleteConflict == nil {
			if prior.deleted {
				return nil, errors.New("remote note changes after delete")
			}
			step.source, step.expected = prior.relative, prior.content
			switch change.Mutation {
			case clientsync.Update:
				if target != prior.relative {
					return nil, errors.New("remote update changes path without move")
				}
			case clientsync.Move:
				if !bytes.Equal(prior.content, blob) {
					return nil, errors.New("remote move blob differs from current state")
				}
				if prior.change.State == "applied" {
					if _, sourceErr := repository.ReadRooted(c.root, step.source, 1); os.IsNotExist(sourceErr) {
						current, targetErr := repository.ReadRooted(c.root, target, clientsync.MaxBlobBytes)
						if targetErr == nil && bytes.Equal(current, blob) {
							step.exists = true
						} else if staged, stagedErr := repository.RootedStagedMoveExists(c.root, step.source, blob); stagedErr != nil || !staged {
							return nil, errors.New("remote move source missing")
						}
					}
				}
			case clientsync.Delete:
				if target != prior.relative || !bytes.Equal(prior.content, blob) {
					return nil, errors.New("remote delete state differs from current note")
				}
				if prior.change.State == "applied" {
					if _, sourceErr := repository.ReadRooted(c.root, step.source, 1); os.IsNotExist(sourceErr) {
						trashed, trashErr := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
						if trashErr == nil && bytes.Equal(trashed, blob) {
							step.exists = true
						} else if staged, stagedErr := repository.RootedStagedMoveExists(c.root, step.source, blob); stagedErr != nil || !staged {
							return nil, errors.New("remote delete source missing")
						}
					}
				}
			case clientsync.Create:
				return nil, errors.New("remote create repeats existing object")
			}
		} else {
			object, objectExists := objects[change.ObjectID]
			switch change.Mutation {
			case clientsync.Create:
				if objectExists && (object.Type != localindex.ObjectNote || object.RelativePath != target) {
					return nil, errors.New("remote create object already exists under another path")
				}
				if !vacatedPaths[portablePathKey(target)] {
					current, readErr := repository.ReadRooted(c.root, target, clientsync.MaxBlobBytes)
					if readErr == nil {
						step.expected, step.exists = current, true
					} else if !errors.Is(readErr, os.ErrNotExist) {
						return nil, readErr
					}
				}
			case clientsync.Update:
				if !objectExists || object.Type != localindex.ObjectNote || object.RelativePath != target {
					return nil, errors.New("remote update object is absent or moved")
				}
				step.source = object.RelativePath
				step.expected, err = repository.ReadRooted(c.root, step.source, clientsync.MaxBlobBytes)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(object.ContentHash, hashBytes(step.expected)) && !bytes.Equal(step.expected, blob) {
					return nil, errors.New("remote update target changed since reconciliation")
				}
			case clientsync.Move:
				if !objectExists || object.Type != localindex.ObjectNote {
					return nil, errors.New("remote move object is absent")
				}
				step.source = object.RelativePath
				step.expected, err = repository.ReadRooted(c.root, step.source, clientsync.MaxBlobBytes)
				if errors.Is(err, os.ErrNotExist) {
					current, targetErr := repository.ReadRooted(c.root, target, clientsync.MaxBlobBytes)
					if targetErr == nil && bytes.Equal(current, blob) {
						step.exists, step.expected = true, blob
					} else {
						staged, stagedErr := repository.RootedStagedMoveExists(c.root, step.source, blob)
						if stagedErr != nil || !staged {
							return nil, errors.New("remote move source missing")
						}
						step.expected = blob
					}
					err = nil
				} else if err != nil {
					return nil, err
				} else if !bytes.Equal(step.expected, blob) {
					return nil, errors.New("remote move source bytes differ")
				} else if step.source == target {
					step.exists = true
				}
			case clientsync.Delete:
				deleteConflict, conflictErr := exactDeleteConflict, error(nil)
				deleteExpected := blob
				if deleteConflict != nil {
					deleteExpected, conflictErr = clientsync.ReadStagedNote(c.root, deleteConflict.SourceHash)
					if conflictErr != nil {
						return nil, conflictErr
					}
				}
				if objectExists {
					if object.Type != localindex.ObjectNote || object.RelativePath != target && (deleteConflict == nil || object.RelativePath != deleteConflict.OriginalRelative) {
						return nil, errors.New("remote delete object state mismatch")
					}
					step.source = object.RelativePath
					if deleteConflict != nil {
						step.source = deleteConflict.OriginalRelative
						if step.source != target {
							if _, targetErr := repository.ReadRooted(c.root, target, 1); targetErr == nil {
								return nil, errors.New("canonical delete target path is occupied")
							} else if !errors.Is(targetErr, os.ErrNotExist) {
								return nil, targetErr
							}
						}
					}
					step.expected, err = repository.ReadRooted(c.root, step.source, clientsync.MaxBlobBytes)
					if errors.Is(err, os.ErrNotExist) {
						trashed, trashErr := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
						if trashErr == nil && bytes.Equal(trashed, deleteExpected) {
							step.exists, step.expected = true, trashed
						} else {
							staged, stagedErr := repository.RootedStagedMoveExists(c.root, step.source, deleteExpected)
							if stagedErr != nil || !staged {
								return nil, errors.New("remote delete source missing without recoverable trash")
							}
							step.expected = deleteExpected
						}
						err = nil
					} else if err != nil {
						return nil, err
					} else if !bytes.Equal(step.expected, deleteExpected) {
						return nil, errors.New("remote delete source bytes differ")
					}
				} else {
					resolvedDelete, resolveErr := store.AlreadyDeletedResolutionMatches(ctx, change)
					if resolveErr != nil {
						return nil, resolveErr
					}
					if resolvedDelete {
						step.exists, step.locallyApplied, step.expected = true, true, deleteExpected
					} else if deleteConflict != nil && evacuatedDeleteConflict {
						evacuated := ".remember/trash/conflicts/" + deleteConflict.OperationID.String() + ".md"
						content, evacuationErr := repository.ReadRooted(c.root, evacuated, clientsync.MaxBlobBytes)
						if evacuationErr != nil || !bytes.Equal(content, deleteExpected) {
							return nil, errors.New("remote delete conflict evacuation unavailable")
						}
						step.exists, step.locallyApplied, step.expected = true, true, deleteExpected
					} else {
						trashed, trashErr := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
						if trashErr == nil && bytes.Equal(trashed, deleteExpected) {
							step.exists, step.expected = true, trashed
						} else {
							staged, stagedErr := repository.RootedStagedMoveExists(c.root, target, deleteExpected)
							if stagedErr == nil && staged {
								step.expected = deleteExpected
							} else {
								matches, matchErr := store.BaselineMatchesOperation(ctx, change.ObjectID, change.Revision, change.OperationID)
								if matchErr != nil || change.State != "applied" || !matches {
									return nil, errors.New("remote delete lacks recoverable trash")
								}
								step.exists, step.locallyApplied, step.expected = true, true, deleteExpected
							}
						}
					}
				}
			}
		}

		oldPath := ""
		if hadPrior {
			oldPath = prior.relative
		} else if object, ok := objects[change.ObjectID]; ok {
			oldPath = object.RelativePath
		}
		if (change.Mutation == clientsync.Move || change.Mutation == clientsync.Delete) && oldPath != "" {
			oldKey := portablePathKey(oldPath)
			delete(pathOwners, oldKey)
			vacatedPaths[oldKey] = true
		}
		pathKey := portablePathKey(step.relative)
		if !step.deleted {
			if existing, ok := pathOwners[pathKey]; ok && existing != change.ObjectID {
				return nil, errors.New("remote plan contains portable path collision")
			}
			pathOwners[pathKey] = change.ObjectID
		}
		if (step.change.Mutation == clientsync.Create || step.change.Mutation == clientsync.Move) && change.State != "applied" && !vacatedPaths[pathKey] {
			if err := rejectPortableSiblingCollisionExcept(c.root, step.relative, step.source); err != nil {
				if change.ParentID == nil || !plannedFolders[*change.ParentID] || !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
			}
		}
		if step.change.Mutation == clientsync.Create && step.exists && !bytes.Equal(step.expected, step.content) {
			return nil, fmt.Errorf("remote create path collision: %s", step.relative)
		}
		if step.deleted {
			delete(objects, change.ObjectID)
		} else {
			object := localindex.Object{ID: change.ObjectID, Type: localindex.ObjectNote, RelativePath: step.relative, CollisionPath: portablePathKey(step.relative), ContentHash: append([]byte(nil), change.BlobHash...), IdentityState: localindex.IdentityKnown}
			if change.ParentID != nil {
				object.ParentID = *change.ParentID
			}
			objects[change.ObjectID] = object
		}
		virtual[change.ObjectID] = step
		prepared = append(prepared, step)
	}
	for _, final := range virtual {
		if final.change.State != "applied" {
			continue
		}
		if err := c.verifyNoteApplyStep(final); err != nil {
			return nil, err
		}
	}
	return prepared, nil
}

func stagedConflictDeferredChange(ctx context.Context, store *clientsync.Store, change clientsync.Change) (*clientsync.ConflictMaterialization, error) {
	if change.ObjectType != clientsync.Note || (change.Mutation != clientsync.Update && change.Mutation != clientsync.Move) {
		return nil, nil
	}
	items, err := store.StagedConflictMaterializations(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.SourceObjectID != change.ObjectID {
			continue
		}
		canonical, err := store.CanonicalConflictState(ctx, item.OperationID)
		if err != nil {
			return nil, err
		}
		if canonical == nil || canonical.ObjectType != clientsync.Note {
			continue
		}
		kind, err := store.ConflictMutationKind(ctx, item.OperationID)
		if err != nil {
			return nil, err
		}
		if kind == clientsync.Move && !canonical.Deleted && canonical.Revision == change.Revision {
			parentMatches := canonical.ParentID == nil && change.ParentID == nil || canonical.ParentID != nil && change.ParentID != nil && *canonical.ParentID == *change.ParentID
			if !parentMatches || canonical.Name != change.Name || !bytes.Equal(canonical.BlobHash, change.BlobHash) || change.Deleted {
				return nil, errors.New("move conflict canonical pull state mismatch")
			}
		}
		if kind == clientsync.Update && canonical.Deleted && canonical.Revision > change.Revision || (kind == clientsync.Delete || kind == clientsync.Move) && !canonical.Deleted && canonical.Revision >= change.Revision {
			copyItem := item
			return &copyItem, nil
		}
	}
	return nil, nil
}

func stagedRemoteDeleteConflict(ctx context.Context, store *clientsync.Store, change clientsync.Change) (*clientsync.ConflictMaterialization, error) {
	items, err := store.StagedConflictMaterializations(ctx)
	if err != nil {
		return nil, err
	}
	var found *clientsync.ConflictMaterialization
	for _, item := range items {
		if item.SourceObjectID != change.ObjectID {
			continue
		}
		canonical, err := store.CanonicalConflictState(ctx, item.OperationID)
		if err != nil {
			return nil, err
		}
		if canonical == nil || canonical.ObjectType != clientsync.Note || !canonical.Deleted || canonical.Revision != change.Revision || canonical.Name != change.Name || !bytes.Equal(canonical.BlobHash, change.BlobHash) || canonical.ParentID == nil && change.ParentID != nil || canonical.ParentID != nil && (change.ParentID == nil || *canonical.ParentID != *change.ParentID) {
			continue
		}
		if found != nil {
			return nil, errors.New("multiple staged delete conflicts for note")
		}
		copyItem := item
		found = &copyItem
	}
	return found, nil
}

func (c *LocalCore) preflightFolderStep(ctx context.Context, store *clientsync.Store, planID uuid.UUID, index int, change clientsync.Change, objects map[uuid.UUID]localindex.Object, pathOwners map[string]uuid.UUID) (preparedNoteStep, error) {
	target, err := remoteNotePath(objects, change.ParentID, change.Name)
	if err != nil {
		return preparedNoteStep{}, err
	}
	step := preparedNoteStep{index: index, change: change, relative: target}
	key := portablePathKey(target)
	if owner, used := pathOwners[key]; used && owner != change.ObjectID {
		return step, errors.New("remote folder path collision")
	}
	if change.Mutation == clientsync.Create {
		publication, err := store.FolderPublication(ctx, planID, index)
		if err != nil {
			return step, err
		}
		if publication != nil {
			if publication.FolderID != change.ObjectID || publication.TargetRelative != target {
				return step, errors.New("folder publication target mismatch")
			}
			step.folderPublication = publication
		} else if existing, ok := objects[change.ObjectID]; ok && existing.Type == localindex.ObjectFolder && existing.RelativePath == target && (change.State == "applied" || clientsync.IsReservedConflictFolder(change.ObjectID)) {
			step.locallyApplied = true
			step.folderPublication = &clientsync.FolderPublication{PlanID: planID, StepIndex: index, FolderID: change.ObjectID, TargetRelative: target, CleanupAuthorized: true, Device: existing.FolderDevice, Inode: existing.FolderInode}
		} else {
			publication, err = c.prepareFolderPublication(ctx, store, planID, index, change.ObjectID, target)
			if err != nil {
				return step, err
			}
			step.folderPublication = publication
		}
		object := localindex.Object{ID: change.ObjectID, Type: localindex.ObjectFolder, RelativePath: target, CollisionPath: key, IdentityState: localindex.IdentityKnown, FolderDevice: step.folderPublication.Device, FolderInode: step.folderPublication.Inode}
		if change.ParentID != nil {
			object.ParentID = *change.ParentID
		}
		objects[change.ObjectID], pathOwners[key] = object, change.ObjectID
		return step, nil
	}
	binding, err := store.FolderMutation(ctx, planID, index)
	if err != nil {
		return step, err
	}
	object, exists := objects[change.ObjectID]
	if change.Mutation == clientsync.Delete && !exists {
		resolved, resolveErr := store.AlreadyDeletedResolutionMatches(ctx, change)
		if resolveErr != nil {
			return step, resolveErr
		}
		if !resolved {
			resolved, resolveErr = store.FolderMoveDeleteRecoveryMatches(ctx, change)
			if resolveErr != nil {
				return step, resolveErr
			}
		}
		if resolved {
			step.deleted, step.locallyApplied, step.conflictDeferred = true, true, true
			return step, nil
		}
	}
	if binding == nil {
		intent, err := store.AcceptedFolderIntent(ctx, change)
		if err != nil {
			return step, err
		}
		bindingTarget := target
		if change.Mutation == clientsync.Delete {
			bindingTarget = ".remember/trash/folders/" + change.ObjectID.String() + "-" + change.OperationID.String()
		}
		if intent != nil {
			if change.Mutation == clientsync.Move && (!exists || object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.RelativePath != target || object.FolderDevice != intent.Device || object.FolderInode != intent.Inode) {
				return step, errors.New("accepted folder move no longer matches local intent")
			}
			if change.Mutation == clientsync.Delete && exists {
				return step, errors.New("accepted folder delete remains locally present")
			}
			binding = &clientsync.FolderMutation{PlanID: planID, StepIndex: index, FolderID: change.ObjectID, Mutation: change.Mutation, SourceRelative: intent.SourceRelative, TargetRelative: bindingTarget, Device: intent.Device, Inode: intent.Inode}
		} else {
			if !exists || object.Type != localindex.ObjectFolder || object.IdentityState != localindex.IdentityKnown || object.FolderDevice == 0 || object.FolderInode == 0 {
				return step, errors.New("remote folder mutation lacks durable inode identity")
			}
			binding = &clientsync.FolderMutation{PlanID: planID, StepIndex: index, FolderID: change.ObjectID, Mutation: change.Mutation, SourceRelative: object.RelativePath, TargetRelative: bindingTarget, Device: object.FolderDevice, Inode: object.FolderInode}
		}
		if err := store.PutFolderMutation(ctx, *binding); err != nil {
			return step, err
		}
	}
	expectedBindingTarget := target
	if change.Mutation == clientsync.Delete {
		expectedBindingTarget = ".remember/trash/folders/" + change.ObjectID.String() + "-" + change.OperationID.String()
	}
	if binding.FolderID != change.ObjectID || binding.Mutation != change.Mutation || binding.TargetRelative != expectedBindingTarget {
		return step, errors.New("folder mutation binding mismatch")
	}
	step.folderMutation, step.source, step.folderDevice, step.folderInode = true, binding.SourceRelative, binding.Device, binding.Inode
	if exists && object.Type != localindex.ObjectFolder {
		return step, errors.New("remote folder mutation object type mismatch")
	}
	if change.Mutation == clientsync.Move && exists && object.RelativePath == target && object.FolderDevice == binding.Device && object.FolderInode == binding.Inode {
		step.locallyApplied = true
		return step, nil
	}
	if change.Mutation == clientsync.Delete && !exists {
		step.deleted, step.trash, step.locallyApplied = true, binding.TargetRelative, true
		return step, nil
	}
	if change.Mutation == clientsync.Move {
		if target == step.source {
			step.locallyApplied = true
			return step, nil
		}
		if strings.HasPrefix(target+"/", step.source+"/") {
			return step, errors.New("invalid remote folder move cycle or no-op")
		}
		oldPrefix := step.source + "/"
		delete(pathOwners, portablePathKey(step.source))
		for id, child := range objects {
			if child.RelativePath != step.source && !strings.HasPrefix(child.RelativePath, oldPrefix) {
				continue
			}
			delete(pathOwners, portablePathKey(child.RelativePath))
			child.RelativePath = target + strings.TrimPrefix(child.RelativePath, step.source)
			child.CollisionPath = portablePathKey(child.RelativePath)
			if id == change.ObjectID && change.ParentID != nil {
				child.ParentID = *change.ParentID
			} else if id == change.ObjectID {
				child.ParentID = uuid.Nil
			}
			objects[id] = child
			pathOwners[portablePathKey(child.RelativePath)] = id
		}
		return step, nil
	}
	if target != step.source {
		return step, errors.New("remote folder delete state mismatch")
	}
	for id, child := range objects {
		if id != change.ObjectID && strings.HasPrefix(child.RelativePath, step.source+"/") {
			return step, errors.New("remote folder delete requires children to be deleted first")
		}
	}
	step.deleted = true
	step.trash = binding.TargetRelative
	delete(pathOwners, portablePathKey(step.source))
	delete(objects, change.ObjectID)
	return step, nil
}

func (c *LocalCore) prepareFolderPublication(ctx context.Context, store *clientsync.Store, planID uuid.UUID, stepIndex int, folderID uuid.UUID, target string) (*clientsync.FolderPublication, error) {
	publication, err := store.FolderPublication(ctx, planID, stepIndex)
	if err != nil {
		return nil, err
	}
	if publication != nil {
		if publication.FolderID != folderID || publication.TargetRelative != target {
			return nil, errors.New("folder publication target mismatch")
		}
		if publication.CleanupAuthorized {
			return publication, nil
		}
		if repository.VerifyRootedFolderPublication(c.root, target, publication.Nonce, publication.Device, publication.Inode) == nil {
			return publication, nil
		}
		if err := repository.VerifyRootedFolderPublication(c.root, publication.StageRelative, publication.Nonce, publication.Device, publication.Inode); err != nil {
			return nil, errors.New("folder publication identity unavailable")
		}
		return publication, nil
	}
	if _, err := os.Lstat(filepath.Join(c.root, filepath.FromSlash(target))); err == nil {
		return nil, errors.New("unbound folder target already exists")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	stage := fmt.Sprintf(".remember/apply/folders/%s/%d", planID.String(), stepIndex)
	if err := repository.RemoveRootedFolderPublicationStage(c.root, stage); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	device, inode, err := repository.CreateRootedFolderPublication(c.root, stage, nonce)
	if err != nil {
		return nil, err
	}
	publication = &clientsync.FolderPublication{PlanID: planID, StepIndex: stepIndex, FolderID: folderID, TargetRelative: target, StageRelative: stage, Nonce: nonce, Device: device, Inode: inode}
	if err := store.PutFolderPublication(ctx, *publication); err != nil {
		return nil, err
	}
	return publication, nil
}

func (c *LocalCore) applyFolderCreateStep(ctx context.Context, store *clientsync.Store, planID uuid.UUID, step preparedNoteStep) error {
	publication := step.folderPublication
	if step.locallyApplied {
		if step.change.State == "applied" {
			return nil
		}
		return store.MarkApplyStepApplied(ctx, planID, step.index)
	}
	if publication.CleanupAuthorized {
		consumed, err := store.FolderPublicationConsumedByDelete(ctx, planID, publication.FolderID)
		if err != nil {
			return err
		}
		if consumed {
			return nil
		}
		return repository.VerifyRootedFolderIdentity(c.root, publication.TargetRelative, publication.Device, publication.Inode)
	}
	if err := repository.VerifyRootedFolderPublication(c.root, publication.TargetRelative, publication.Nonce, publication.Device, publication.Inode); err != nil {
		if testHookBeforeFolderPublication != nil {
			testHookBeforeFolderPublication()
		}
		if err := repository.PublishRootedFolderPublication(c.root, publication.StageRelative, publication.TargetRelative, publication.Nonce, publication.Device, publication.Inode); err != nil {
			return err
		}
	}
	if err := repository.VerifyRootedFolderPublication(c.root, publication.TargetRelative, publication.Nonce, publication.Device, publication.Inode); err != nil {
		return err
	}
	before, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	verify := func() error {
		return repository.VerifyRootedFolderPublication(c.root, publication.TargetRelative, publication.Nonce, publication.Device, publication.Inode)
	}
	if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{
		RecoveryMode: c.recoveryMode, AppliedRemoteFolderPaths: map[uuid.UUID]string{publication.FolderID: publication.TargetRelative}, TrustedRemoteFolders: map[string]uuid.UUID{publication.TargetRelative: publication.FolderID}, VerifyTrustedRemoteFolders: verify,
	}); err != nil {
		return err
	}
	if testHookAfterFolderReconcile != nil {
		testHookAfterFolderReconcile()
	}
	if err := verify(); err != nil {
		if restoreErr := c.index.ReplaceSnapshot(ctx, before); restoreErr != nil {
			return fmt.Errorf("folder publication changed and snapshot restore failed: %v / %w", err, restoreErr)
		}
		return err
	}
	return store.MarkFolderStepAppliedAndAuthorizeCleanup(ctx, planID, step.index)
}

func (c *LocalCore) applyFolderMutationStep(ctx context.Context, store *clientsync.Store, planID uuid.UUID, step preparedNoteStep) error {
	if step.change.State == "applied" || step.locallyApplied {
		if step.deleted {
			if _, _, err := repository.RootedFolderIdentity(c.root, step.source); err == nil {
				return errors.New("deleted folder source was recreated")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else if err := repository.VerifyRootedFolderIdentity(c.root, step.relative, step.folderDevice, step.folderInode); err != nil {
			return err
		}
		if step.change.State == "applied" {
			return nil
		}
		return store.MarkApplyStepApplied(ctx, planID, step.index)
	}
	if step.deleted {
		publication, err := store.FolderPublicationForFolder(ctx, planID, step.change.ObjectID)
		if err != nil {
			return err
		}
		if publication != nil {
			if publication.Device != step.folderDevice || publication.Inode != step.folderInode {
				return errors.New("folder delete publication identity mismatch")
			}
			consumed, err := store.FolderPublicationConsumedByDelete(ctx, planID, step.change.ObjectID)
			if err != nil {
				return err
			}
			if !consumed {
				if err := repository.CleanupRootedFolderPublication(c.root, step.source, publication.Nonce, publication.Device, publication.Inode); err != nil {
					return err
				}
				if err := store.MarkFolderPublicationConsumedByDelete(ctx, planID, step.change.ObjectID); err != nil {
					return err
				}
			}
		}
		if err := repository.DeleteRootedFolderExpected(c.root, step.source, step.folderDevice, step.folderInode); err != nil {
			return err
		}
	} else if err := repository.MoveRootedFolderExpected(c.root, step.source, step.relative, step.folderDevice, step.folderInode); err != nil {
		return err
	}
	if testHookAfterFolderMutationPublication != nil {
		if err := testHookAfterFolderMutationPublication(); err != nil {
			return err
		}
	}
	verify := func() error {
		if step.deleted {
			if _, _, err := repository.RootedFolderIdentity(c.root, step.source); err == nil {
				return errors.New("deleted folder source was recreated")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		return repository.VerifyRootedFolderIdentity(c.root, step.relative, step.folderDevice, step.folderInode)
	}
	if err := verify(); err != nil {
		return err
	}
	before, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	options := reconcile.Options{RecoveryMode: c.recoveryMode, VerifyTrustedRemoteFolders: verify}
	if step.deleted {
		options.AppliedRemoteDeletes = map[uuid.UUID]bool{step.change.ObjectID: true}
		options.TrustedRemoteFolderDeletes = map[string]uuid.UUID{step.source: step.change.ObjectID}
	} else {
		options.AppliedRemoteFolders = map[uuid.UUID]bool{step.change.ObjectID: true}
		options.AppliedRemoteFolderPaths = make(map[uuid.UUID]string)
		options.AppliedRemoteNotes = make(map[uuid.UUID][32]byte)
		options.AppliedRemoteNotePaths = make(map[uuid.UUID]string)
		prefix := step.source + "/"
		for _, object := range before.Objects {
			if object.RelativePath != step.source && !strings.HasPrefix(object.RelativePath, prefix) {
				continue
			}
			targetPath := step.relative + strings.TrimPrefix(object.RelativePath, step.source)
			if object.Type == localindex.ObjectFolder {
				options.AppliedRemoteFolders[object.ID] = true
				options.AppliedRemoteFolderPaths[object.ID] = targetPath
			} else if len(object.ContentHash) == sha256.Size {
				var hash [sha256.Size]byte
				copy(hash[:], object.ContentHash)
				options.AppliedRemoteNotes[object.ID] = hash
				options.AppliedRemoteNotePaths[object.ID] = targetPath
			}
		}
		options.TrustedRemoteFolderMoves = map[string]string{step.source: step.relative}
	}
	if _, err := reconcile.Run(ctx, c.root, c.index, options); err != nil {
		return err
	}
	if err := verify(); err != nil {
		if restoreErr := c.index.ReplaceSnapshot(ctx, before); restoreErr != nil {
			return fmt.Errorf("folder mutation changed and snapshot restore failed: %v / %w", err, restoreErr)
		}
		return err
	}
	return store.MarkApplyStepApplied(ctx, planID, step.index)
}

func (c *LocalCore) cleanupCompletedFolderPublications(ctx context.Context, store *clientsync.Store) error {
	publications, err := store.CompletedFolderPublications(ctx)
	if err != nil {
		return err
	}
	snapshot, err := c.index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	folderPaths := make(map[uuid.UUID]string)
	for _, object := range snapshot.Objects {
		if object.Type == localindex.ObjectFolder {
			folderPaths[object.ID] = object.RelativePath
		}
	}
	for _, publication := range publications {
		target := publication.TargetRelative
		if current := folderPaths[publication.FolderID]; current != "" {
			target = current
		} else if finalTarget, found, err := store.LatestFolderMutationTarget(ctx, publication.PlanID, publication.FolderID); err != nil {
			return err
		} else if found {
			target = finalTarget
		}
		if err := repository.CleanupRootedFolderPublication(c.root, target, publication.Nonce, publication.Device, publication.Inode); err != nil {
			return err
		}
		if err := store.MarkFolderPublicationCleaned(ctx, publication.PlanID, publication.StepIndex); err != nil {
			return err
		}
	}
	return nil
}

func (c *LocalCore) publishNoteApplyStep(step preparedNoteStep) error {
	switch step.change.Mutation {
	case clientsync.Create:
		if step.exists {
			return nil
		}
		return repository.CreateRooted(c.root, step.relative, step.content, validateAppliedNote(step.change.ObjectID))
	case clientsync.Update:
		current, err := repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		if bytes.Equal(current, step.content) {
			return nil
		}
		if !bytes.Equal(current, step.expected) {
			return errors.New("remote update source changed")
		}
		return repository.WriteRootedExpected(c.root, step.relative, step.expected, step.content, validateAppliedNote(step.change.ObjectID))
	case clientsync.Move:
		if step.exists {
			return nil
		}
		return repository.MoveRootedExpected(c.root, step.source, step.relative, step.expected)
	case clientsync.Delete:
		if step.exists {
			return nil
		}
		if err := repository.EnsureRootedDirectory(c.root, ".remember/trash", 0o700); err != nil {
			return err
		}
		return repository.MoveRootedExpected(c.root, step.source, step.trash, step.expected)
	default:
		return ErrUnsupportedApplyPlan
	}
}

func (c *LocalCore) verifyNoteApplyStep(step preparedNoteStep) error {
	if step.deleted {
		if _, err := repository.ReadRooted(c.root, step.relative, 1); !os.IsNotExist(err) {
			return errors.New("deleted note still exists")
		}
		if step.locallyApplied {
			return nil
		}
		trashed, err := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
		if err != nil || !bytes.Equal(trashed, step.expected) {
			return errors.New("recoverable remote note delete missing")
		}
		return nil
	}
	current, err := repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
	if err != nil || !bytes.Equal(current, step.content) {
		return errors.New("applied remote note bytes differ")
	}
	return nil
}

func remoteNotePath(objects map[uuid.UUID]localindex.Object, parentID *uuid.UUID, name string) (string, error) {
	if parentID == nil {
		return name, nil
	}
	parent, ok := objects[*parentID]
	if !ok || parent.Type != localindex.ObjectFolder {
		return "", errors.New("remote note parent is unavailable")
	}
	return path.Join(parent.RelativePath, name), nil
}

func portablePathKey(relative string) string {
	parts := strings.Split(relative, "/")
	for index := range parts {
		parts[index] = naming.CollisionKey(parts[index])
	}
	return strings.Join(parts, "/")
}

func rejectPortableSiblingCollision(root, relative string) error {
	return rejectPortableSiblingCollisionExcept(root, relative, "")
}

func rejectPortableSiblingCollisionExcept(root, relative, allowedSource string) error {
	directory := path.Dir(relative)
	absolute := root
	if directory != "." {
		absolute = filepath.Join(root, filepath.FromSlash(directory))
	}
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return err
	}
	base := path.Base(relative)
	key := naming.CollisionKey(base)
	for _, entry := range entries {
		candidate := path.Join(directory, entry.Name())
		if directory == "." {
			candidate = entry.Name()
		}
		if candidate == allowedSource || entry.Name() == base {
			continue
		}
		if naming.CollisionKey(entry.Name()) == key {
			return errors.New("remote note has a portable sibling collision")
		}
	}
	return nil
}

func hashBytes(content []byte) []byte {
	hash := sha256.Sum256(content)
	return hash[:]
}

func validateAppliedNote(expected uuid.UUID) repository.Validator {
	return func(content []byte) error {
		inspection, err := frontmatter.Inspect(content)
		if err != nil {
			return err
		}
		if !inspection.HasRemember || inspection.NoteID != expected {
			return errors.New("applied note identity mismatch")
		}
		return nil
	}
}
