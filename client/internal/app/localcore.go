// Package app exposes the headless local-core use cases used by a future UI.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/watcher"
	"github.com/gofrs/flock"
)

const recoveryModeStateKey = "folder_identity_recovery_mode"

var (
	ErrNotInitialized     = errors.New("remember root is not initialized")
	ErrAlreadyInitialized = errors.New("remember root is already initialized")
	ErrRootInUse          = errors.New("remember root is already open")
	ErrCoreClosed         = errors.New("local core is closed")
	ErrWatcherStarted     = errors.New("filesystem watcher already started")
)

// Update is one watcher-triggered full reconciliation result.
type Update struct {
	Hint   watcher.Event
	Report reconcile.Report
	Err    error
}

// LocalCore owns one root, its local index, and optional watcher.
type LocalCore struct {
	root         string
	index        *localindex.Index
	rootLock     *flock.Flock
	recoveryMode bool

	reconcileMu sync.Mutex
	lifecycleMu sync.Mutex
	source      *watcher.Source
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closed      bool
}

// Initialize adopts a previously unmanaged root and may create initial folder
// identities. It refuses roots that already contain a technical directory.
func Initialize(ctx context.Context, root string) (*LocalCore, reconcile.Report, error) {
	absolute, err := validateRoot(root)
	if err != nil {
		return nil, reconcile.Report{}, err
	}
	technical := filepath.Join(absolute, ".remember")
	if err := os.Mkdir(technical, 0o700); err != nil {
		if os.IsExist(err) {
			return nil, reconcile.Report{}, ErrAlreadyInitialized
		}
		return nil, reconcile.Report{}, fmt.Errorf("create technical directory: %w", err)
	}
	rootLock, err := acquireRootLock(filepath.Join(technical, "lock"))
	if err != nil {
		return nil, reconcile.Report{}, err
	}
	index, err := localindex.Open(ctx, filepath.Join(technical, "index.db"))
	if err != nil {
		rootLock.Unlock()
		return nil, reconcile.Report{}, err
	}
	core := &LocalCore{root: absolute, index: index, rootLock: rootLock}
	report, err := reconcile.Run(ctx, absolute, index, reconcile.Options{AllowInitialFolderIDs: true})
	if err != nil {
		index.Close()
		rootLock.Unlock()
		return nil, reconcile.Report{}, err
	}
	return core, report, nil
}

// Open opens an initialized root without guessing identities after index loss.
func Open(ctx context.Context, root string) (*LocalCore, reconcile.Report, error) {
	absolute, err := validateRoot(root)
	if err != nil {
		return nil, reconcile.Report{}, err
	}
	technical := filepath.Join(absolute, ".remember")
	technicalInfo, err := os.Lstat(technical)
	if os.IsNotExist(err) {
		return nil, reconcile.Report{}, ErrNotInitialized
	}
	if err != nil {
		return nil, reconcile.Report{}, fmt.Errorf("inspect technical directory: %w", err)
	}
	if technicalInfo.Mode()&os.ModeSymlink != 0 || !technicalInfo.IsDir() {
		return nil, reconcile.Report{}, fmt.Errorf("technical path is not a real directory")
	}

	indexPath := filepath.Join(technical, "index.db")
	info, err := os.Lstat(indexPath)
	recoveryMode := os.IsNotExist(err)
	if err != nil && !recoveryMode {
		return nil, reconcile.Report{}, fmt.Errorf("inspect local index: %w", err)
	}
	if !recoveryMode && !info.Mode().IsRegular() {
		return nil, reconcile.Report{}, fmt.Errorf("local index is not a regular file")
	}

	rootLock, err := acquireRootLock(filepath.Join(technical, "lock"))
	if err != nil {
		return nil, reconcile.Report{}, err
	}
	index, err := localindex.Open(ctx, indexPath)
	if err != nil {
		rootLock.Unlock()
		return nil, reconcile.Report{}, err
	}
	if recoveryMode {
		if err := index.SetState(ctx, recoveryModeStateKey, "1"); err != nil {
			index.Close()
			rootLock.Unlock()
			return nil, reconcile.Report{}, err
		}
	} else {
		value, exists, err := index.State(ctx, recoveryModeStateKey)
		if err != nil {
			index.Close()
			rootLock.Unlock()
			return nil, reconcile.Report{}, err
		}
		recoveryMode = exists && value == "1"
	}
	core := &LocalCore{root: absolute, index: index, rootLock: rootLock, recoveryMode: recoveryMode}
	report, err := core.Reconcile(ctx)
	if err != nil {
		index.Close()
		rootLock.Unlock()
		return nil, reconcile.Report{}, err
	}
	return core, report, nil
}

// Root returns the absolute managed root path.
func (c *LocalCore) Root() string { return c.root }

// Reconcile performs a complete scan; watcher events are merely triggers.
func (c *LocalCore) Reconcile(ctx context.Context) (reconcile.Report, error) {
	return c.reconcileWithOptions(ctx, reconcile.Options{RecoveryMode: c.recoveryMode})
}

func (c *LocalCore) reconcileWithOptions(ctx context.Context, options reconcile.Options) (reconcile.Report, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.lifecycleMu.Lock()
	closed := c.closed
	c.lifecycleMu.Unlock()
	if closed {
		return reconcile.Report{}, ErrCoreClosed
	}
	return reconcile.Run(ctx, c.root, c.index, options)
}

// Snapshot returns current reconstructable metadata and local issues.
func (c *LocalCore) Snapshot(ctx context.Context) (localindex.Snapshot, error) {
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.lifecycleMu.Lock()
	closed := c.closed
	c.lifecycleMu.Unlock()
	if closed {
		return localindex.Snapshot{}, ErrCoreClosed
	}
	return c.index.ReadSnapshot(ctx)
}

// StartWatching starts one event loop. Every hint causes a complete,
// serialized reconciliation; no event sequence is assumed complete.
func (c *LocalCore) StartWatching(parent context.Context) (<-chan Update, error) {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed {
		return nil, errors.New("local core is closed")
	}
	if c.source != nil {
		return nil, ErrWatcherStarted
	}
	source, err := watcher.Open(c.root)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c.source = source
	c.cancel = cancel
	updates := make(chan Update, 32)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer close(updates)
		defer source.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case hint, ok := <-source.Events():
				if !ok {
					return
				}
				options := reconcile.Options{RecoveryMode: c.recoveryMode}
				if hint.Kind == watcher.MoveCandidate && hint.RelativePath != "" {
					options.MoveCandidates = []string{hint.RelativePath}
				}
				report, err := c.reconcileWithOptions(ctx, options)
				select {
				case updates <- Update{Hint: hint, Report: report, Err: err}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return updates, nil
}

// Close stops watching before closing the index.
func (c *LocalCore) Close() error {
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		return nil
	}
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
	c.lifecycleMu.Unlock()
	c.wg.Wait()
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	indexErr := c.index.Close()
	unlockErr := c.rootLock.Unlock()
	if indexErr != nil {
		return indexErr
	}
	return unlockErr
}

func acquireRootLock(path string) (*flock.Flock, error) {
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock remember root: %w", err)
	}
	if !locked {
		return nil, ErrRootInUse
	}
	return lock, nil
}

func validateRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("managed root must not be a symlink")
	}
	if !info.IsDir() {
		return "", fmt.Errorf("managed root is not a directory")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve root ancestors: %w", err)
	}
	return resolved, nil
}
