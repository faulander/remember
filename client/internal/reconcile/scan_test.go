package reconcile

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestScanInventoriesTreeWithoutWriting(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	id := "550e8400-e29b-41d4-a716-446655440000"
	files := map[string][]byte{
		"Note.md":             []byte("# no identity\n"),
		"Folder/With ID.MD":   []byte("---\nremember:\n  schema: 1\n  note_id: \"" + id + "\"\n---\nBody\n"),
		"Folder/duplicate.md": []byte("---\nremember:\n  schema: 1\n  note_id: \"" + id + "\"\n---\nOther\n"),
		"Folder/broken.md":    []byte("---\n[invalid\n---\n"),
		"ignored.txt":         []byte("not a note"),
		".remember/index.db":  []byte("technical"),
	}
	for relative, content := range files {
		writeTestFile(t, root, relative, content)
	}
	if err := os.Mkdir(filepath.Join(root, "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, root)
	first, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	second, err := Scan(root)
	if err != nil {
		t.Fatalf("second Scan() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("Scan() is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	after := snapshotTree(t, root)
	if !reflect.DeepEqual(before, after) {
		t.Error("Scan() modified the filesystem")
	}

	paths := make([]string, 0, len(first.Entries))
	for _, entry := range first.Entries {
		paths = append(paths, entry.RelativePath)
	}
	wantPaths := []string{"Empty", "Folder", "Folder/With ID.MD", "Folder/broken.md", "Folder/duplicate.md", "Note.md"}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Errorf("entry paths = %q, want %q", paths, wantPaths)
	}

	var duplicateCount, invalidFrontmatterCount int
	for _, issue := range first.Issues {
		switch issue.Code {
		case IssueDuplicateNoteID:
			duplicateCount++
		case IssueInvalidFrontmatter:
			invalidFrontmatterCount++
		}
	}
	if duplicateCount != 2 {
		t.Errorf("duplicate issue count = %d, want 2", duplicateCount)
	}
	if invalidFrontmatterCount != 1 {
		t.Errorf("invalid frontmatter issue count = %d, want 1", invalidFrontmatterCount)
	}

	for _, entry := range first.Entries {
		if entry.RelativePath == "Note.md" {
			if want := sha256.Sum256(files["Note.md"]); entry.ContentHash != want {
				t.Errorf("Note.md hash = %x, want %x", entry.ContentHash, want)
			}
		}
	}
}

func TestScanReportsSymlinkWithoutFollowingIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires environment-specific privileges on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	writeTestFile(t, outside, "outside.md", []byte("secret"))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	inventory, err := Scan(root)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(inventory.Entries) != 0 {
		t.Errorf("symlink target was inventoried: %#v", inventory.Entries)
	}
	if len(inventory.Issues) != 1 || inventory.Issues[0].Code != IssueSymlink {
		t.Errorf("issues = %#v, want one symlink issue", inventory.Issues)
	}
}

func TestAppendNameCollisionIssues(t *testing.T) {
	t.Parallel()

	inventory := Inventory{Entries: []Entry{
		{Type: EntryNote, RelativePath: "Folder/Note.md"},
		{Type: EntryNote, RelativePath: "Folder/note.MD"},
		{Type: EntryNote, RelativePath: "Other/Note.md"},
	}}
	appendNameCollisionIssues(&inventory)
	if len(inventory.Issues) != 2 {
		t.Fatalf("collision issues = %#v, want two", inventory.Issues)
	}
	if inventory.Issues[0].Code != IssueNameCollision || inventory.Issues[1].Code != IssueNameCollision {
		t.Errorf("issues = %#v, want name collisions", inventory.Issues)
	}
}

func TestReservedTechnicalCaseVariantIsReported(t *testing.T) {
	t.Parallel()

	if err := validateScannedPath(".REMEMBER/Note.md"); err == nil {
		t.Error("case variant of technical root was accepted")
	}
	if err := validateScannedPath(".remember/Note.md"); err != nil {
		t.Errorf("actual technical root path rejected internally: %v", err)
	}
}

func TestScanRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires environment-specific privileges on Windows")
	}
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(link); err == nil {
		t.Error("Scan() accepted a symlink root")
	}
}

func TestScanRejectsNonDirectoryRoot(t *testing.T) {
	t.Parallel()

	file := filepath.Join(t.TempDir(), "note.md")
	if err := os.WriteFile(file, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(file); err == nil {
		t.Error("Scan() accepted a file as root")
	}
}

func writeTestFile(t *testing.T, root, relative string, content []byte) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func snapshotTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[filepath.ToSlash(relative)+"/"] = nil
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[filepath.ToSlash(relative)] = []byte("symlink:" + target)
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = bytes.Clone(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
