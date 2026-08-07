package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/remotehttp"
	"github.com/google/uuid"
)

const maxForegroundPullPages = 32

var (
	ErrUnresolvedOutbound  = errors.New("local sync operations require resolution")
	ErrUnsupportedPullPage = errors.New("pull page contains unsupported changes")
)

type SyncRemote interface {
	clientsync.BlobResolver
	PutBlob(context.Context, [sha256.Size]byte, []byte) error
	Submit(context.Context, clientsync.Mutation) (clientsync.Result, error)
	Pull(context.Context, uint64, int) (remotehttp.PullPage, error)
}

// SyncOnce runs one bounded foreground cycle. Credentials remain owned by the
// injected remote and are never written to the root or local index.
func (c *LocalCore) SyncOnce(ctx context.Context, remote SyncRemote) error {
	if remote == nil {
		return errors.New("nil sync remote")
	}
	c.reconcileMu.Lock()
	defer c.reconcileMu.Unlock()
	c.lifecycleMu.Lock()
	closed := c.closed
	c.lifecycleMu.Unlock()
	if closed {
		return ErrCoreClosed
	}
	store, err := clientsync.NewStore(c.index)
	if err != nil {
		return err
	}
	if err := c.stageSupportedConflicts(ctx, store); err != nil {
		return err
	}
	if err := c.publishStagedConflicts(ctx, store); err != nil {
		return err
	}
	if err := c.cleanupCompletedFolderPublications(ctx, store); err != nil {
		return err
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil {
		return err
	} else if active != nil {
		if err := c.executeActiveApplyPlanLocked(ctx, remote); err != nil {
			return err
		}
	}
	if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode}); err != nil {
		return err
	}
	for {
		ready, err := store.ListReady(ctx, 1)
		if err != nil {
			return err
		}
		if len(ready) == 0 {
			break
		}
		item := ready[0]
		if err := store.MarkAttempted(ctx, item.Mutation.OperationID); err != nil {
			return err
		}
		if item.Mutation.ObjectType == clientsync.Note && (item.Mutation.Kind == clientsync.Create || item.Mutation.Kind == clientsync.Update) {
			var hash [sha256.Size]byte
			copy(hash[:], item.Mutation.BlobHash)
			content, err := clientsync.ReadStagedNote(c.root, hash)
			if err != nil {
				return err
			}
			if err := remote.PutBlob(ctx, hash, content); err != nil {
				return err
			}
		}
		result, err := remote.Submit(ctx, item.Mutation)
		if errors.Is(err, remotehttp.ErrReplayMismatch) {
			_ = store.RecordReplayMismatch(ctx, item.Mutation.OperationID)
			return err
		}
		if err != nil {
			return err
		}
		if err := store.RecordResult(ctx, item.Mutation.OperationID, result); err != nil {
			return err
		}
		if !result.Accepted {
			if err := c.stageSupportedConflicts(ctx, store); err != nil {
				return err
			}
		}
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil {
		return err
	} else if unresolved {
		return ErrUnresolvedOutbound
	}
	for pageNumber := 0; pageNumber < maxForegroundPullPages; pageNumber++ {
		after, err := store.ConfirmedCursor(ctx)
		if err != nil {
			return err
		}
		page, err := remote.Pull(ctx, after, 100)
		if err != nil {
			return err
		}
		if len(page.Changes) == 0 {
			if page.HasMore || page.NextCursor != after {
				return remotehttp.ErrInvalidResponse
			}
			return nil
		}
		expected := after
		for _, change := range page.Changes {
			expected++
			if change.Cursor != expected {
				return remotehttp.ErrInvalidResponse
			}
			if change.ObjectType == clientsync.Folder {
				validMutation := change.Mutation == clientsync.Create || change.Mutation == clientsync.Move || change.Mutation == clientsync.Delete
				validRevision := change.Mutation == clientsync.Create && change.Revision == 1 || change.Mutation != clientsync.Create && change.Revision >= 2
				if !validMutation || !validRevision || change.Deleted != (change.Mutation == clientsync.Delete) || len(change.BlobHash) != 0 {
					return ErrUnsupportedPullPage
				}
			} else if change.ObjectType != clientsync.Note || (change.Mutation != clientsync.Create && change.Mutation != clientsync.Update && change.Mutation != clientsync.Move && change.Mutation != clientsync.Delete) {
				return ErrUnsupportedPullPage
			}
		}
		if page.NextCursor != expected || page.NextCursor <= after {
			return remotehttp.ErrInvalidResponse
		}
		planID, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("generate apply plan id: %w", err)
		}
		if err := store.CreateApplyPlan(ctx, clientsync.ApplyPlan{ID: planID, FromCursor: after, ThroughCursor: page.NextCursor, Steps: page.Changes}); err != nil {
			return err
		}
		if err := c.executeActiveApplyPlanLocked(ctx, remote); err != nil {
			return err
		}
		if err := c.publishStagedConflicts(ctx, store); err != nil {
			return err
		}
		if !page.HasMore {
			return nil
		}
	}
	return errors.New("foreground pull page bound exceeded")
}
