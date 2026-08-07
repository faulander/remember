package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

const MaxNoteBytes = 8 * 1024 * 1024

var (
	ErrInvalidNotePath   = errors.New("invalid note path")
	ErrInvalidFolderPath = errors.New("invalid folder path")
	ErrNoteTooLarge      = errors.New("note exceeds 8 MiB")
	ErrNoteNotFound      = errors.New("note not found")
	ErrDestinationUsed   = errors.New("destination already exists")
)

// NoteDocument is the editable projection returned to the desktop UI.
type NoteDocument struct {
	ID           uuid.UUID
	RelativePath string
	Body         string
	Tags         []string
	Revision     string
}

func (c *LocalCore) ReadNote(ctx context.Context, relative string) (NoteDocument, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return NoteDocument{}, err
	}
	if _, err := c.resolveNote(relative); err != nil {
		return NoteDocument{}, err
	}
	return c.readNoteFileRooted(filepath.ToSlash(relative))
}

func (c *LocalCore) CreateNote(ctx context.Context, relative string, body string, tags []string) (NoteDocument, reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if err := c.ensureNoActiveApplyPlan(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if _, err := c.resolveNewNote(relative, ""); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if len(body) > MaxNoteBytes {
		return NoteDocument{}, reconcile.Report{}, ErrNoteTooLarge
	}
	candidate, err := frontmatter.NewNoteID()
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, fmt.Errorf("generate note id: %w", err)
	}
	identity, err := frontmatter.EnsureIdentity([]byte(body), candidate)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	content, err := frontmatter.UpdateEditable(identity.Markdown, candidate, []byte(body), tags)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if len(content) > MaxNoteBytes {
		return NoteDocument{}, reconcile.Report{}, ErrNoteTooLarge
	}
	if err := repository.CreateRooted(c.root, filepath.ToSlash(relative), content, validateNote(candidate)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return NoteDocument{}, reconcile.Report{}, ErrDestinationUsed
		}
		return NoteDocument{}, reconcile.Report{}, err
	}
	report, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode})
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	document, err := c.readNoteFileRooted(filepath.ToSlash(relative))
	return document, report, err
}

// CreateFolder creates one real, empty folder below an existing real parent.
func (c *LocalCore) CreateFolder(ctx context.Context, relative string) (reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return reconcile.Report{}, err
	}
	if err := c.ensureNoActiveApplyPlan(ctx); err != nil {
		return reconcile.Report{}, err
	}
	if err := c.resolveNewFolder(relative); err != nil {
		return reconcile.Report{}, err
	}
	if err := repository.CreateRootedDirectory(c.root, filepath.ToSlash(relative), 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return reconcile.Report{}, ErrDestinationUsed
		}
		return reconcile.Report{}, err
	}
	return reconcile.Run(ctx, c.root, c.index, reconcile.Options{
		RecoveryMode: c.recoveryMode, TrustedNewFolders: []string{filepath.ToSlash(relative)},
	})
}

func (c *LocalCore) SaveNote(ctx context.Context, relative, expectedRevision, body string, tags []string) (NoteDocument, reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if err := c.ensureNoActiveApplyPlan(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if len(body) > MaxNoteBytes {
		return NoteDocument{}, reconcile.Report{}, ErrNoteTooLarge
	}
	if _, err := c.resolveNote(relative); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	original, err := c.readBoundedRooted(filepath.ToSlash(relative))
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if revision(original) != expectedRevision {
		return NoteDocument{}, reconcile.Report{}, repository.ErrConcurrentModification
	}
	editable, err := frontmatter.Read(original)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if err := c.rejectActiveNoteConflict(ctx, editable.NoteID); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	content, err := frontmatter.UpdateEditable(original, editable.NoteID, []byte(body), tags)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if len(content) > MaxNoteBytes {
		return NoteDocument{}, reconcile.Report{}, ErrNoteTooLarge
	}
	if err := repository.WriteRootedExpected(c.root, filepath.ToSlash(relative), original, content, validateNote(editable.NoteID)); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	report, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode})
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	document, err := c.readNoteFileRooted(filepath.ToSlash(relative))
	return document, report, err
}

func (c *LocalCore) MoveNote(ctx context.Context, sourceRelative, destinationRelative, expectedRevision string) (NoteDocument, reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if err := c.ensureNoActiveApplyPlan(ctx); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	source, err := c.resolveNote(sourceRelative)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	original, err := c.readBoundedRooted(filepath.ToSlash(sourceRelative))
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if revision(original) != expectedRevision {
		return NoteDocument{}, reconcile.Report{}, repository.ErrConcurrentModification
	}
	editable, err := frontmatter.Read(original)
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, fmt.Errorf("read stable note identity: %w", err)
	}
	if editable.NoteID == uuid.Nil {
		return NoteDocument{}, reconcile.Report{}, errors.New("note has no stable identity")
	}
	if err := c.rejectActiveNoteConflict(ctx, editable.NoteID); err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	destination, err := c.resolveNewNote(destinationRelative, filepath.ToSlash(sourceRelative))
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	if source == destination {
		document, err := c.readNoteFileRooted(filepath.ToSlash(sourceRelative))
		return document, reconcile.Report{}, err
	}
	if err := repository.MoveRootedExpected(c.root, filepath.ToSlash(sourceRelative), filepath.ToSlash(destinationRelative), original); err != nil {
		if errors.Is(err, os.ErrExist) {
			return NoteDocument{}, reconcile.Report{}, ErrDestinationUsed
		}
		return NoteDocument{}, reconcile.Report{}, err
	}
	report, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode})
	if err != nil {
		return NoteDocument{}, reconcile.Report{}, err
	}
	document, err := c.readNoteFileRooted(filepath.ToSlash(destinationRelative))
	if err == nil && document.ID != editable.NoteID {
		return NoteDocument{}, reconcile.Report{}, errors.New("note identity changed during move")
	}
	return document, report, err
}

func (c *LocalCore) DeleteNote(ctx context.Context, relative, expectedRevision string) (reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	if err := c.ensureUsable(ctx); err != nil {
		return reconcile.Report{}, err
	}
	if err := c.ensureNoActiveApplyPlan(ctx); err != nil {
		return reconcile.Report{}, err
	}
	if _, err := c.resolveNote(relative); err != nil {
		return reconcile.Report{}, err
	}
	original, err := c.readBoundedRooted(filepath.ToSlash(relative))
	if err != nil {
		return reconcile.Report{}, err
	}
	if revision(original) != expectedRevision {
		return reconcile.Report{}, repository.ErrConcurrentModification
	}
	document, err := frontmatter.Read(original)
	if err != nil {
		return reconcile.Report{}, fmt.Errorf("read stable note identity: %w", err)
	}
	if document.NoteID == uuid.Nil {
		return reconcile.Report{}, errors.New("note has no stable identity")
	}
	if err := c.rejectActiveNoteConflict(ctx, document.NoteID); err != nil {
		return reconcile.Report{}, err
	}
	if err := repository.EnsureRootedDirectory(c.root, ".remember/trash", 0o700); err != nil {
		return reconcile.Report{}, fmt.Errorf("%w: create secure note trash: %v", ErrInvalidNotePath, err)
	}
	for {
		destinationRelative := ".remember/trash/" + document.NoteID.String() + "-" + uuid.NewString() + ".md"
		err = repository.MoveRootedExpected(c.root, filepath.ToSlash(relative), destinationRelative, original)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return reconcile.Report{}, err
		}
	}
	return reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode})
}

func (c *LocalCore) ensureUsable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed {
		return ErrCoreClosed
	}
	return nil
}

func (c *LocalCore) resolveNote(relative string) (string, error) {
	if err := validateNoteRelative(relative); err != nil {
		return "", err
	}
	path := filepath.Join(c.root, filepath.FromSlash(relative))
	if err := validateRealParents(c.root, path, false); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", ErrNoteNotFound
	}
	if err != nil {
		return "", fmt.Errorf("inspect note: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", ErrInvalidNotePath
	}
	return path, nil
}

func (c *LocalCore) resolveNewNote(relative, sourceRelative string) (string, error) {
	if err := validateNoteRelative(relative); err != nil {
		return "", err
	}
	path := filepath.Join(c.root, filepath.FromSlash(relative))
	if err := validateRealParents(c.root, path, true); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("read destination folder: %w", err)
	}
	base := filepath.Base(path)
	for _, entry := range entries {
		existingRelative, _ := filepath.Rel(c.root, filepath.Join(filepath.Dir(path), entry.Name()))
		if filepath.ToSlash(existingRelative) == sourceRelative {
			continue
		}
		if naming.CollisionKey(entry.Name()) == naming.CollisionKey(base) {
			return "", ErrDestinationUsed
		}
	}
	return path, nil
}

func (c *LocalCore) resolveNewFolder(relative string) error {
	if filepath.IsAbs(relative) || relative != filepath.ToSlash(relative) || naming.ValidateUserRelativePath(relative) != nil {
		return ErrInvalidFolderPath
	}
	path := filepath.Join(c.root, filepath.FromSlash(relative))
	if err := validateRealParents(c.root, path, true); err != nil {
		return ErrInvalidFolderPath
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("read folder parent: %w", err)
	}
	base := filepath.Base(path)
	for _, entry := range entries {
		if naming.CollisionKey(entry.Name()) == naming.CollisionKey(base) {
			return ErrDestinationUsed
		}
	}
	return nil
}

func validateNoteRelative(relative string) error {
	if filepath.IsAbs(relative) || relative != filepath.ToSlash(relative) || naming.ValidateUserRelativePath(relative) != nil ||
		!strings.EqualFold(filepath.Ext(relative), ".md") {
		return ErrInvalidNotePath
	}
	return nil
}

func validateRealParents(root, destination string, destinationMayNotExist bool) error {
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ErrInvalidNotePath
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	limit := len(parts)
	if destinationMayNotExist || limit > 0 {
		limit--
	}
	for i := 0; i < limit; i++ {
		current = filepath.Join(current, parts[i])
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrInvalidNotePath
		}
	}
	return nil
}

func (c *LocalCore) readNoteFileRooted(relative string) (NoteDocument, error) {
	content, err := c.readBoundedRooted(relative)
	if err != nil {
		return NoteDocument{}, err
	}
	editable, err := frontmatter.Read(content)
	if err != nil {
		return NoteDocument{}, err
	}
	if editable.NoteID == uuid.Nil {
		return NoteDocument{}, errors.New("note has no stable identity")
	}
	return NoteDocument{
		ID: editable.NoteID, RelativePath: relative, Body: string(editable.Body),
		Tags: editable.Tags, Revision: revision(content),
	}, nil
}

func (c *LocalCore) readBoundedRooted(relative string) ([]byte, error) {
	content, err := repository.ReadRooted(c.root, relative, MaxNoteBytes)
	if errors.Is(err, repository.ErrContentTooLarge) {
		return nil, ErrNoteTooLarge
	}
	return content, err
}

func revision(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

func validateNote(expectedID uuid.UUID) repository.Validator {
	return func(content []byte) error {
		inspection, err := frontmatter.Inspect(content)
		if err != nil {
			return err
		}
		if inspection.NoteID != expectedID {
			return errors.New("staged note identity changed")
		}
		return nil
	}
}
