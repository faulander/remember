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
	index    int
	change   clientsync.Change
	relative string
	expected []byte
	content  []byte
	exists   bool
}

// ExecuteActiveApplyPlan resumes the one durable remote ApplyPlan. This slice
// deliberately supports note Create and Update only and validates the entire
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
		if step.exists {
			current, err := repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
			if err != nil {
				return err
			}
			if !bytes.Equal(current, step.content) {
				if step.change.Mutation == clientsync.Create {
					return fmt.Errorf("remote create destination differs: %s", step.relative)
				}
				if !bytes.Equal(current, step.expected) {
					return fmt.Errorf("remote update source changed: %s", step.relative)
				}
				if err := repository.WriteRootedExpected(c.root, step.relative, step.expected, step.content, validateAppliedNote(step.change.ObjectID)); err != nil {
					return err
				}
			}
		} else {
			if step.change.Mutation != clientsync.Create {
				return fmt.Errorf("remote update target is missing: %s", step.relative)
			}
			if err := repository.CreateRooted(c.root, step.relative, step.content, validateAppliedNote(step.change.ObjectID)); err != nil {
				return err
			}
		}
		if testHookAfterApplyPublication != nil {
			testHookAfterApplyPublication()
		}
		if err := rejectPortableSiblingCollision(c.root, step.relative); err != nil {
			return err
		}
		appliedHash := sha256.Sum256(step.content)
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: map[uuid.UUID][32]byte{step.change.ObjectID: appliedHash}}); err != nil {
			return err
		}
		// Close the filesystem/SQLite gap: if another process edited the note
		// during the suppressed scan, do not journal or advance the cursor.
		current, err := repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
		if err != nil || !bytes.Equal(current, step.content) {
			return errors.New("remote note changed during apply reconciliation")
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
		if step.ObjectType != clientsync.Note || (step.Mutation != clientsync.Create && step.Mutation != clientsync.Update) || step.Deleted || len(step.BlobHash) != sha256.Size {
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
			return nil, errors.New("invalid remote note update")
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
	plannedPaths := make(map[string]uuid.UUID)
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
		step := preparedNoteStep{index: index, change: change, content: blob}
		if prior, ok := virtual[change.ObjectID]; ok {
			target, pathErr := remoteNotePath(objects, change.ParentID, change.Name)
			if pathErr != nil || target != prior.relative {
				return nil, errors.New("remote note state path changes without a move")
			}
			step.relative, step.expected, step.exists = prior.relative, prior.content, true
		} else if change.State == "applied" {
			// Historical journaled bytes need not still be on disk when a later
			// step for the same object was published before a crash. Seed the
			// virtual chain from the authenticated step; the final applied state
			// is checked against disk after the whole plan is prepared.
			step.relative, err = remoteNotePath(objects, change.ParentID, change.Name)
			if err != nil {
				return nil, err
			}
			if object, ok := objects[change.ObjectID]; ok && object.RelativePath != step.relative {
				return nil, errors.New("applied remote note path mismatches local object")
			}
			step.expected, step.exists = step.content, true
		} else if change.Mutation == clientsync.Update {
			object, ok := objects[change.ObjectID]
			if !ok || object.Type != localindex.ObjectNote {
				return nil, errors.New("remote update object is absent from local index")
			}
			serverPath, pathErr := remoteNotePath(objects, change.ParentID, change.Name)
			if pathErr != nil || serverPath != object.RelativePath {
				return nil, errors.New("remote update state path mismatches local object")
			}
			step.relative = object.RelativePath
			step.expected, err = repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(object.ContentHash, hashBytes(step.expected)) && !bytes.Equal(step.expected, step.content) {
				return nil, errors.New("remote update target changed since reconciliation")
			}
			step.exists = true
		} else {
			step.relative, err = remoteNotePath(objects, change.ParentID, change.Name)
			if err != nil {
				return nil, err
			}
			if err := rejectPortableSiblingCollision(c.root, step.relative); err != nil {
				return nil, err
			}
			if object, exists := objects[change.ObjectID]; exists && (object.Type != localindex.ObjectNote || object.RelativePath != step.relative) {
				return nil, errors.New("remote create object already exists under another path")
			}
			current, readErr := repository.ReadRooted(c.root, step.relative, clientsync.MaxBlobBytes)
			if readErr == nil {
				step.expected, step.exists = current, true
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return nil, readErr
			}
		}
		pathKey := portablePathKey(step.relative)
		if existing, ok := plannedPaths[pathKey]; ok && existing != change.ObjectID {
			return nil, errors.New("remote plan contains a portable path collision")
		}
		plannedPaths[pathKey] = change.ObjectID
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
		current, err := repository.ReadRooted(c.root, final.relative, clientsync.MaxBlobBytes)
		if err != nil || !bytes.Equal(current, final.content) {
			return nil, errors.New("applied remote step no longer matches canonical bytes")
		}
	}
	return prepared, nil
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
		if entry.Name() != base && naming.CollisionKey(entry.Name()) == key {
			return errors.New("remote create has a portable sibling collision")
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
