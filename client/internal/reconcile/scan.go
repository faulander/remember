// Package reconcile inventories local files without modifying them.
package reconcile

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

// EntryType distinguishes synchronized filesystem objects.
type EntryType string

const (
	EntryNote   EntryType = "note"
	EntryFolder EntryType = "folder"
)

// Entry is one read-only inventory record.
type Entry struct {
	Type         EntryType
	RelativePath string
	NoteID       uuid.UUID
	ContentHash  [sha256.Size]byte
}

// IssueCode identifies a local problem that must be shown rather than fixed
// silently.
type IssueCode string

const (
	IssueInvalidName        IssueCode = "invalid_name"
	IssueNameCollision      IssueCode = "name_collision"
	IssueInvalidFrontmatter IssueCode = "invalid_frontmatter"
	IssueDuplicateNoteID    IssueCode = "duplicate_note_id"
	IssueUnreadable         IssueCode = "unreadable"
	IssueSymlink            IssueCode = "unsupported_symlink"
)

// Issue describes a local-only problem. RelativePath may be shown locally but
// must not be sent in telemetry.
type Issue struct {
	Code         IssueCode
	RelativePath string
	Detail       string
}

// Inventory is sorted by logical relative path and has deterministic issues.
type Inventory struct {
	Entries []Entry
	Issues  []Issue
}

// Scan builds a read-only inventory. It never follows symlinks and excludes
// Remember's root technical directory.
func Scan(root string) (Inventory, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve scan root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return Inventory{}, fmt.Errorf("stat root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Inventory{}, fmt.Errorf("scan root must not be a symlink")
	}
	if !info.IsDir() {
		return Inventory{}, fmt.Errorf("scan root is not a directory")
	}
	root, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return Inventory{}, fmt.Errorf("resolve scan root ancestors: %w", err)
	}

	var inventory Inventory
	err = filepath.WalkDir(root, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if fullPath == root {
			return walkErr
		}
		relative, relErr := filepath.Rel(root, fullPath)
		if relErr != nil {
			return relErr
		}
		relative = filepath.ToSlash(relative)

		if walkErr != nil {
			inventory.Issues = append(inventory.Issues, Issue{
				Code: IssueUnreadable, RelativePath: relative, Detail: walkErr.Error(),
			})
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if isTechnicalRoot(relative) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			inventory.Issues = append(inventory.Issues, Issue{Code: IssueSymlink, RelativePath: relative})
			return nil
		}

		if entry.IsDir() {
			if err := validateScannedPath(relative); err != nil {
				inventory.Issues = append(inventory.Issues, Issue{
					Code: IssueInvalidName, RelativePath: relative, Detail: err.Error(),
				})
			}
			inventory.Entries = append(inventory.Entries, Entry{Type: EntryFolder, RelativePath: relative})
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		if err := validateScannedPath(relative); err != nil {
			inventory.Issues = append(inventory.Issues, Issue{
				Code: IssueInvalidName, RelativePath: relative, Detail: err.Error(),
			})
		}

		content, readErr := os.ReadFile(fullPath)
		if readErr != nil {
			inventory.Issues = append(inventory.Issues, Issue{
				Code: IssueUnreadable, RelativePath: relative, Detail: readErr.Error(),
			})
			return nil
		}
		note := Entry{Type: EntryNote, RelativePath: relative, ContentHash: sha256.Sum256(content)}
		inspection, inspectErr := frontmatter.Inspect(content)
		if inspectErr != nil {
			inventory.Issues = append(inventory.Issues, Issue{
				Code: IssueInvalidFrontmatter, RelativePath: relative, Detail: inspectErr.Error(),
			})
		} else {
			note.NoteID = inspection.NoteID
		}
		inventory.Entries = append(inventory.Entries, note)
		return nil
	})
	if err != nil {
		return Inventory{}, fmt.Errorf("walk root: %w", err)
	}

	appendDuplicateIssues(&inventory)
	appendNameCollisionIssues(&inventory)
	sort.Slice(inventory.Entries, func(i, j int) bool {
		return inventory.Entries[i].RelativePath < inventory.Entries[j].RelativePath
	})
	sort.Slice(inventory.Issues, func(i, j int) bool {
		if inventory.Issues[i].RelativePath == inventory.Issues[j].RelativePath {
			return inventory.Issues[i].Code < inventory.Issues[j].Code
		}
		return inventory.Issues[i].RelativePath < inventory.Issues[j].RelativePath
	})
	return inventory, nil
}

func isTechnicalRoot(relative string) bool {
	first, _, _ := strings.Cut(relative, "/")
	return first == ".remember"
}

func validateScannedPath(relative string) error {
	if err := naming.ValidateRelativePath(relative); err != nil {
		return err
	}
	first, _, _ := strings.Cut(relative, "/")
	if first != ".remember" && naming.CollisionKey(first) == naming.CollisionKey(".remember") {
		return fmt.Errorf("root name %q collides with reserved .remember", first)
	}
	if first != "_Konflikte" && naming.CollisionKey(first) == naming.CollisionKey("_Konflikte") {
		return fmt.Errorf("root name %q collides with reserved _Konflikte", first)
	}
	return nil
}

func appendNameCollisionIssues(inventory *Inventory) {
	byParent := make(map[string][]string)
	for _, entry := range inventory.Entries {
		if validateScannedPath(entry.RelativePath) != nil {
			continue
		}
		parent := path.Dir(entry.RelativePath)
		byParent[parent] = append(byParent[parent], path.Base(entry.RelativePath))
	}
	for parent, names := range byParent {
		collisions, err := naming.FindSiblingCollisions(names)
		if err != nil {
			continue
		}
		for _, collision := range collisions {
			for _, name := range collision.Names {
				relative := name
				if parent != "." {
					relative = path.Join(parent, name)
				}
				inventory.Issues = append(inventory.Issues, Issue{
					Code: IssueNameCollision, RelativePath: relative, Detail: collision.Key,
				})
			}
		}
	}
}

func appendDuplicateIssues(inventory *Inventory) {
	pathsByID := make(map[uuid.UUID][]string)
	for _, entry := range inventory.Entries {
		if entry.Type == EntryNote && entry.NoteID != uuid.Nil {
			pathsByID[entry.NoteID] = append(pathsByID[entry.NoteID], entry.RelativePath)
		}
	}
	for id, paths := range pathsByID {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		for _, relative := range paths {
			inventory.Issues = append(inventory.Issues, Issue{
				Code: IssueDuplicateNoteID, RelativePath: relative, Detail: id.String(),
			})
		}
	}
}
