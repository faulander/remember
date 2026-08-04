// Package watcher turns platform filesystem events into reconciliation hints.
package watcher

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// Kind is a normalized hint. Events are never treated as complete truth.
type Kind string

const (
	Create         Kind = "create"
	Modify         Kind = "modify"
	Remove         Kind = "remove"
	MoveCandidate  Kind = "move_candidate"
	RescanRequired Kind = "rescan_required"
)

// Event is a root-relative filesystem hint.
type Event struct {
	Kind         Kind
	RelativePath string
	Err          error
}

// Source recursively watches existing and newly created directories.
type Source struct {
	root    string
	watcher *fsnotify.Watcher
	events  chan Event
	done    chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

// Open starts a recursive watcher. Its first event always requests a full
// rescan, making watcher startup independent of races during registration.
func Open(root string) (*Source, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve watcher root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, fmt.Errorf("stat watcher root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("watcher root must not be a symlink")
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("watcher root is not a directory")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, fmt.Errorf("resolve watcher root ancestors: %w", err)
	}

	backend, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create filesystem watcher: %w", err)
	}
	source := &Source{
		root: absolute, watcher: backend,
		events: make(chan Event, 128), done: make(chan struct{}),
	}
	if err := source.addTree(absolute); err != nil {
		backend.Close()
		return nil, err
	}
	source.emit(Event{Kind: RescanRequired})
	source.wg.Add(1)
	go source.run()
	return source, nil
}

// Events returns normalized hints until Close.
func (s *Source) Events() <-chan Event { return s.events }

// Close stops the source and closes Events.
func (s *Source) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		err = s.watcher.Close()
		s.wg.Wait()
	})
	return err
}

func (s *Source) run() {
	defer s.wg.Done()
	defer close(s.events)
	for {
		select {
		case <-s.done:
			return
		case raw, ok := <-s.watcher.Events:
			if !ok {
				return
			}
			if raw.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(raw.Name); err == nil && info.IsDir() {
					if err := s.addTree(raw.Name); err != nil {
						s.emit(Event{Kind: RescanRequired, Err: err})
					}
				}
			}
			kind, relevant := normalize(raw.Op)
			if relevant {
				s.emit(Event{Kind: kind, RelativePath: s.relative(raw.Name)})
			}
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
			s.emit(Event{Kind: RescanRequired, Err: err})
		}
	}
}

func (s *Source) emit(event Event) {
	select {
	case s.events <- event:
	case <-s.done:
	}
}

func (s *Source) relative(path string) string {
	relative, err := filepath.Rel(s.root, path)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(relative)
}

func (s *Source) addTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		if path != s.root {
			relative, err := filepath.Rel(s.root, path)
			if err != nil {
				return err
			}
			first, _, _ := strings.Cut(filepath.ToSlash(relative), "/")
			if first == ".remember" {
				return filepath.SkipDir
			}
		}
		if err := s.watcher.Add(path); err != nil {
			return fmt.Errorf("watch directory %q: %w", path, err)
		}
		return nil
	})
}

func normalize(operation fsnotify.Op) (Kind, bool) {
	switch {
	case operation&fsnotify.Rename != 0:
		return MoveCandidate, true
	case operation&fsnotify.Remove != 0:
		return Remove, true
	case operation&fsnotify.Create != 0:
		return Create, true
	case operation&fsnotify.Write != 0:
		return Modify, true
	default:
		return "", false
	}
}
