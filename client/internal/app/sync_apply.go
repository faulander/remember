package app

import (
	"bytes"
	"context"
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

type preparedNoteStep struct {
	index             int
	change            clientsync.Change
	relative, source  string
	trash             string
	expected, content []byte
	exists, deleted   bool
	locallyApplied    bool
}

// ExecuteActiveApplyPlan resumes the one durable remote ApplyPlan. This slice
// deliberately supports note Create, Update, Move and Delete and validates the entire
// pending plan before publishing any bytes.
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
	for _, step := range steps {
		if step.change.State == "applied" {
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
		options := reconcile.Options{RecoveryMode: c.recoveryMode}
		if step.deleted {
			options.AppliedRemoteDeletes = map[uuid.UUID]bool{step.change.ObjectID: true}
		} else {
			options.AppliedRemoteNotes = map[uuid.UUID][32]byte{step.change.ObjectID: sha256.Sum256(step.content)}
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, options); err != nil {
			return err
		}
		if err := c.verifyNoteApplyStep(step); err != nil {
			return err
		}
		if err := store.MarkApplyStepApplied(ctx, plan.ID, step.index); err != nil {
			return err
		}
	}
	return store.CompleteApplyPlan(ctx, plan.ID)
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
	for _, step := range plan.Steps {
		if step.ObjectType != clientsync.Note || (step.Mutation != clientsync.Create && step.Mutation != clientsync.Update && step.Mutation != clientsync.Move && step.Mutation != clientsync.Delete) || step.Deleted != (step.Mutation == clientsync.Delete) || len(step.BlobHash) != sha256.Size {
			return nil, ErrUnsupportedApplyPlan
		}
		if !strings.EqualFold(path.Ext(step.Name), ".md") {
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
		if change.Mutation == clientsync.Delete {
			step.trash = ".remember/trash/" + change.ObjectID.String() + "-" + change.OperationID.String() + ".md"
		}
		prior, hadPrior := virtual[change.ObjectID]
		if change.State == "applied" && lastStep[change.ObjectID] > index {
			step.source, step.expected, step.exists = target, blob, true
		} else if hadPrior {
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
				if objectExists {
					if object.Type != localindex.ObjectNote || object.RelativePath != target {
						return nil, errors.New("remote delete object state mismatch")
					}
					step.source = object.RelativePath
					step.expected, err = repository.ReadRooted(c.root, step.source, clientsync.MaxBlobBytes)
					if errors.Is(err, os.ErrNotExist) {
						trashed, trashErr := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
						if trashErr == nil && bytes.Equal(trashed, blob) {
							step.exists = true
						} else {
							staged, stagedErr := repository.RootedStagedMoveExists(c.root, step.source, blob)
							if stagedErr != nil || !staged {
								return nil, errors.New("remote delete source missing without recoverable trash")
							}
						}
						step.expected, err = blob, nil
					} else if err != nil {
						return nil, err
					} else if !bytes.Equal(step.expected, blob) {
						return nil, errors.New("remote delete source bytes differ")
					}
				} else {
					trashed, trashErr := repository.ReadRooted(c.root, step.trash, clientsync.MaxBlobBytes)
					if trashErr == nil && bytes.Equal(trashed, blob) {
						step.exists = true
					} else {
						staged, stagedErr := repository.RootedStagedMoveExists(c.root, target, blob)
						if stagedErr == nil && staged {
							step.exists = false
						} else {
							matches, matchErr := store.BaselineMatchesOperation(ctx, change.ObjectID, change.Revision, change.OperationID)
							if matchErr != nil || change.State != "applied" || !matches {
								return nil, errors.New("remote delete lacks recoverable trash")
							}
							step.exists, step.locallyApplied = true, true
						}
					}
					step.expected = blob
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
				return nil, err
			}
		}
		if step.change.Mutation == clientsync.Create && step.exists && !bytes.Equal(step.expected, step.content) {
			return nil, fmt.Errorf("remote create path collision: %s", step.relative)
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
		if err != nil || !bytes.Equal(trashed, step.content) {
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
