//go:build darwin || linux

package repository

import (
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

type subtreeFixture struct {
	root    string
	device  uint64
	inode   uint64
	entries []RootedSubtreeEntry
}

func newSubtreeFixture(t *testing.T) subtreeFixture {
	t.Helper()
	root := t.TempDir()
	for _, relative := range []string{"Tree/A", "Tree/A/B"} {
		if err := os.MkdirAll(filepath.Join(root, relative), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{"Tree/root.md": []byte("root\n"), "Tree/A/B/deep.md": []byte("deep\n")}
	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(root, relative), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	device, inode, err := RootedFolderIdentity(root, "Tree")
	if err != nil {
		t.Fatal(err)
	}
	aDevice, aInode, err := RootedFolderIdentity(root, "Tree/A")
	if err != nil {
		t.Fatal(err)
	}
	bDevice, bInode, err := RootedFolderIdentity(root, "Tree/A/B")
	if err != nil {
		t.Fatal(err)
	}
	return subtreeFixture{root: root, device: device, inode: inode, entries: []RootedSubtreeEntry{
		{Relative: "A", Kind: RootedSubtreeFolder, Device: aDevice, Inode: aInode},
		{Relative: "A/B", Kind: RootedSubtreeFolder, Device: bDevice, Inode: bInode},
		{Relative: "A/B/deep.md", Kind: RootedSubtreeFile, Hash: sha256.Sum256(files["Tree/A/B/deep.md"])},
		{Relative: "root.md", Kind: RootedSubtreeFile, Hash: sha256.Sum256(files["Tree/root.md"])},
	}}
}

func TestVerifyRootedSubtreeExpectedThreeLevels(t *testing.T) {
	f := newSubtreeFixture(t)
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 1024); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRootedSubtreeExpectedRejectsManifestAndContentMismatches(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *subtreeFixture)
	}{
		{"unexpected", func(t *testing.T, f *subtreeFixture) {
			if err := os.WriteFile(filepath.Join(f.root, "Tree", "extra"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing", func(t *testing.T, f *subtreeFixture) { f.entries = f.entries[:len(f.entries)-1] }},
		{"type", func(t *testing.T, f *subtreeFixture) {
			if err := os.Remove(filepath.Join(f.root, "Tree", "root.md")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(f.root, "Tree", "root.md"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"hash", func(t *testing.T, f *subtreeFixture) {
			if err := os.WriteFile(filepath.Join(f.root, "Tree", "root.md"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"inode", func(t *testing.T, f *subtreeFixture) { f.entries[0].Inode++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSubtreeFixture(t)
			test.mutate(t, &f)
			if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 1024); err == nil {
				t.Fatal("mismatch accepted")
			}
		})
	}
	f := newSubtreeFixture(t)
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode+1, f.entries, 1024); err == nil {
		t.Fatal("root inode mismatch accepted")
	}
}

func TestVerifyRootedSubtreeExpectedRejectsSocket(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "remember-subtree-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Mkdir(filepath.Join(root, "Tree"), 0o755); err != nil {
		t.Fatal(err)
	}
	device, inode, err := RootedFolderIdentity(root, "Tree")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "Tree", "entry")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	entries := []RootedSubtreeEntry{{Relative: "entry", Kind: RootedSubtreeFile, Hash: sha256.Sum256(nil)}}
	if err := VerifyRootedSubtreeExpected(root, "Tree", device, inode, entries, 1024); err == nil {
		t.Fatal("socket accepted")
	}
}

func TestVerifyRootedSubtreeExpectedRejectsSymlinks(t *testing.T) {
	for _, level := range []string{"file", "folder", "root"} {
		t.Run(level, func(t *testing.T) {
			f := newSubtreeFixture(t)
			outside := filepath.Join(f.root, "outside")
			if err := os.Mkdir(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			switch level {
			case "file":
				if err := os.Remove(filepath.Join(f.root, "Tree", "A", "B", "deep.md")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(outside, "x"), filepath.Join(f.root, "Tree", "A", "B", "deep.md")); err != nil {
					t.Fatal(err)
				}
			case "folder":
				if err := os.RemoveAll(filepath.Join(f.root, "Tree", "A", "B")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(f.root, "Tree", "A", "B")); err != nil {
					t.Fatal(err)
				}
			case "root":
				if err := os.Rename(filepath.Join(f.root, "Tree"), filepath.Join(f.root, "Old")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(f.root, "Old"), filepath.Join(f.root, "Tree")); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 1024); err == nil {
				t.Fatal("symlink accepted")
			}
		})
	}
}

func TestVerifyRootedSubtreeExpectedRejectsOversizeAndInvalidPaths(t *testing.T) {
	f := newSubtreeFixture(t)
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 3); !errors.Is(err, ErrContentTooLarge) {
		t.Fatalf("oversize err=%v", err)
	}
	for _, entries := range [][]RootedSubtreeEntry{
		{{Relative: "../x", Kind: RootedSubtreeFile}}, {{Relative: "A//x", Kind: RootedSubtreeFile}}, {{Relative: "A/x", Kind: RootedSubtreeFile}},
		{{Relative: "A", Kind: RootedSubtreeFolder, Device: 1, Inode: 1}, {Relative: "A", Kind: RootedSubtreeFolder, Device: 1, Inode: 1}},
		{{Relative: "X", Kind: "other"}}, {{Relative: "X", Kind: RootedSubtreeFolder}},
		{{Relative: "X", Kind: RootedSubtreeFile, Device: 1}},
		{{Relative: "X", Kind: RootedSubtreeFolder, Device: 1, Inode: 1, Hash: sha256.Sum256([]byte("x"))}},
	} {
		if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, entries, 1024); err == nil {
			t.Fatalf("invalid manifest accepted: %#v", entries)
		}
	}
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, nil, -1); err == nil {
		t.Fatal("negative bound accepted")
	}
	if err := VerifyRootedSubtreeExpected(f.root, "../Tree", f.device, f.inode, nil, 1); err == nil {
		t.Fatal("invalid root path accepted")
	}
}

func TestVerifyRootedSubtreeExpectedRetainsRootDescriptorAcrossReplacement(t *testing.T) {
	f := newSubtreeFixture(t)
	testHookAfterRootedSubtreeOpen = func() {
		testHookAfterRootedSubtreeOpen = nil
		if err := os.Rename(filepath.Join(f.root, "Tree"), filepath.Join(f.root, "Original")); err != nil {
			panic(err)
		}
		if err := os.Mkdir(filepath.Join(f.root, "Tree"), 0o755); err != nil {
			panic(err)
		}
		if err := os.WriteFile(filepath.Join(f.root, "Tree", "evil"), []byte("evil"), 0o600); err != nil {
			panic(err)
		}
	}
	defer func() { testHookAfterRootedSubtreeOpen = nil }()
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "Tree", "evil")); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRootedSubtreeExpectedNestedRecoveryShapeWithSibling(t *testing.T) {
	f := newSubtreeFixture(t)
	if err := os.Mkdir(filepath.Join(f.root, "Tree", "Sibling"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("sibling\n")
	if err := os.WriteFile(filepath.Join(f.root, "Tree", "Sibling", "other.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	device, inode, err := RootedFolderIdentity(f.root, "Tree/Sibling")
	if err != nil {
		t.Fatal(err)
	}
	f.entries = append(f.entries,
		RootedSubtreeEntry{Relative: "Sibling", Kind: RootedSubtreeFolder, Device: device, Inode: inode},
		RootedSubtreeEntry{Relative: "Sibling/other.md", Kind: RootedSubtreeFile, Hash: sha256.Sum256(content)},
	)
	if err := VerifyRootedSubtreeExpected(f.root, "Tree", f.device, f.inode, f.entries, 1024); err != nil {
		t.Fatal(err)
	}
}
