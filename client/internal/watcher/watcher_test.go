package watcher

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		op       fsnotify.Op
		wantKind Kind
		wantOK   bool
	}{
		{op: fsnotify.Create, wantKind: Create, wantOK: true},
		{op: fsnotify.Write, wantKind: Modify, wantOK: true},
		{op: fsnotify.Remove, wantKind: Remove, wantOK: true},
		{op: fsnotify.Rename, wantKind: MoveCandidate, wantOK: true},
		{op: fsnotify.Chmod, wantOK: false},
		{op: fsnotify.Create | fsnotify.Write, wantKind: Create, wantOK: true},
		{op: fsnotify.Rename | fsnotify.Remove, wantKind: MoveCandidate, wantOK: true},
	}
	for _, tt := range tests {
		gotKind, gotOK := normalize(tt.op)
		if gotKind != tt.wantKind || gotOK != tt.wantOK {
			t.Errorf("normalize(%v) = (%q, %t), want (%q, %t)", tt.op, gotKind, gotOK, tt.wantKind, tt.wantOK)
		}
	}
}

func TestSourceRequestsStartupRescanAndObservesChanges(t *testing.T) {
	root := t.TempDir()
	source, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer source.Close()

	startup := receiveEvent(t, source.Events())
	if startup.Kind != RescanRequired {
		t.Fatalf("first event = %#v, want rescan", startup)
	}

	path := filepath.Join(root, "Note.md")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPathEvent(t, source.Events(), "Note.md")

	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForPathEvent(t, source.Events(), "Note.md")

	if err := os.Rename(path, filepath.Join(root, "Moved.md")); err != nil {
		t.Fatal(err)
	}
	waitForAnyPathEvent(t, source.Events(), map[string]bool{"Note.md": true, "Moved.md": true})
}

func TestSourceWatchesNewDirectoriesAndExcludesTechnicalTree(t *testing.T) {
	root := t.TempDir()
	source, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	<-source.Events()

	folder := filepath.Join(root, "Folder")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	waitForPathEvent(t, source.Events(), "Folder")

	// Allow the directory create handler to register the new watch.
	deadline := time.Now().Add(2 * time.Second)
	for {
		path := filepath.Join(folder, "Nested.md")
		if err := os.WriteFile(path, []byte(time.Now().String()), 0o644); err != nil {
			t.Fatal(err)
		}
		if receivePathUntil(source.Events(), "Folder/Nested.md", 100*time.Millisecond) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new directory was not watched")
		}
	}
}

func TestSourceRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires environment-specific privileges on Windows")
	}
	t.Parallel()

	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Error("Open() accepted a symlink root")
	}
}

func TestSourceRejectsFileRoot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "Note.md")
	if err := os.WriteFile(path, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Error("Open() accepted a file root")
	}
}

func receiveEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("event channel closed")
		}
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for filesystem event")
		return Event{}
	}
}

func waitForPathEvent(t *testing.T, events <-chan Event, relative string) {
	t.Helper()
	waitForAnyPathEvent(t, events, map[string]bool{relative: true})
}

func waitForAnyPathEvent(t *testing.T, events <-chan Event, wanted map[string]bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("event channel closed")
			}
			if wanted[event.RelativePath] {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for one of paths %v", wanted)
		}
	}
}

func receivePathUntil(events <-chan Event, relative string, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return false
			}
			if event.RelativePath == relative {
				return true
			}
		case <-timer.C:
			return false
		}
	}
}
