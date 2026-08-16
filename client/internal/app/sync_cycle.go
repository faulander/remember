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

const (
	maxForegroundPullPages     = 32
	maxIndependentInboxApplies = 32
	maxIndependentInboxScan    = 1000
)

var testHookAfterInboxIngest func() error

var (
	ErrUnresolvedOutbound  = errors.New("local sync operations require resolution")
	ErrUnsupportedPullPage = errors.New("pull page contains unsupported changes")
)

type SyncRemote interface {
	clientsync.BlobResolver
	PutBlob(context.Context, [sha256.Size]byte, []byte) error
	Submit(context.Context, clientsync.Mutation) (clientsync.Result, error)
	Pull(context.Context, uint64, int) (remotehttp.PullPage, error)
	PreserveAndDeleteEmptyFolder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uint64, uint64, uint64) (remotehttp.PreserveDeleteFolderResult, error)
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
	if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
		return err
	}
	awaitingDivergentTreePull := false
	if err := c.stageSupportedConflicts(ctx, store, remote); err != nil {
		if !errors.Is(err, errDivergentTreeAwaitingPull) {
			return err
		}
		awaitingDivergentTreePull = true
	}
	if err := c.cleanupCompletedConflictStages(ctx, store); err != nil {
		return err
	}
	if err := c.cleanupCompletedOutboxBlobs(ctx, store); err != nil {
		return err
	}
	if err := c.publishStagedConflicts(ctx, store, remote); err != nil {
		return err
	}
	if err := c.cleanupCompletedFolderPublications(ctx, store); err != nil {
		return err
	}
	if active, err := store.ActiveApplyPlan(ctx); err != nil {
		return err
	} else if active != nil {
		steps := make([]clientsync.Change, len(active.Steps))
		copy(steps, active.Steps)
		for i := range steps {
			steps[i].State = ""
		}
		if err := store.IngestPullPage(ctx, active.FromCursor, active.ThroughCursor, steps); err != nil {
			return err
		}
		if err := c.executeActiveApplyPlanLocked(ctx, remote); err != nil {
			return err
		}
		if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
			return err
		}
		if err := c.publishStagedConflicts(ctx, store, remote); err != nil {
			return err
		}
	}
	if !awaitingDivergentTreePull {
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode}); err != nil {
			return err
		}
	}
	// Pull authenticated history before dependency-ready child deletes. If it
	// proves that a pending subtree root was remotely moved, probe that root
	// delete first so preserve-and-delete can atomically supersede exact child
	// delete intents before any of them reaches the server.
	if blocked, probeErr := store.PendingBlockedFolderDeletes(ctx); probeErr != nil {
		return probeErr
	} else if len(blocked) > 0 {
		matched, lookErr := ingestPreserveDeleteProbeHistory(ctx, store, remote, blocked)
		if lookErr != nil {
			return lookErr
		}
		if !matched {
			goto normalOutbox
		}
		probe, probeErr := store.PendingPreserveDeleteProbe(ctx)
		if probeErr != nil {
			return probeErr
		}
		if probe != nil {
			if probeErr = store.MarkPreserveDeleteProbeAttempted(ctx, probe.Mutation.OperationID); probeErr != nil {
				return probeErr
			}
			result, submitErr := remote.Submit(ctx, probe.Mutation)
			if submitErr != nil {
				return submitErr
			}
			if probeErr = store.RecordResult(ctx, probe.Mutation.OperationID, result); probeErr != nil {
				return probeErr
			}
			if !result.Accepted {
				if probeErr = c.stageSupportedConflicts(ctx, store, remote); probeErr != nil {
					return probeErr
				}
				still, checkErr := store.HasUnresolvedOutbox(ctx)
				if checkErr != nil {
					return checkErr
				}
				if still {
					return ErrUnresolvedOutbound
				}
			}
		}
	}
normalOutbox:
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
			if err := c.stageSupportedConflicts(ctx, store, remote); err != nil {
				if !errors.Is(err, errDivergentTreeAwaitingPull) {
					return err
				}
				break
			}
			// A rejected subtree-root delete must pull its canonical move before
			// any queued descendant deletes can be submitted.
			if item.Mutation.ObjectType == clientsync.Folder && item.Mutation.Kind == clientsync.Delete && result.Conflict == "base_revision_mismatch" {
				break
			}
		}
	}
	if err := c.publishStagedConflicts(ctx, store, remote); err != nil {
		return err
	}
	if err := c.cleanupCompletedOutboxBlobs(ctx, store); err != nil {
		return err
	}
	if unresolved, err := store.HasUnresolvedOutbox(ctx); err != nil {
		return err
	} else if unresolved {
		if err := c.ingestPullWhileOutboundBlocked(ctx, store, remote); err != nil {
			return err
		}
		if err := c.drainIndependentInbox(ctx, store, remote); err != nil {
			return err
		}
		return ErrUnresolvedOutbound
	}
	for pageNumber := 0; pageNumber < maxForegroundPullPages; pageNumber++ {
		after, err := store.ConfirmedCursor(ctx)
		if err != nil {
			return err
		}
		pullLimit := 100
		downloaded, downloadErr := store.DownloadedCursor(ctx)
		if downloadErr != nil {
			return downloadErr
		}
		if after < downloaded && downloaded-after < uint64(pullLimit) {
			pullLimit = int(downloaded - after)
		}
		page, err := remote.Pull(ctx, after, pullLimit)
		if err != nil {
			return err
		}
		if len(page.Changes) == 0 {
			if page.HasMore || page.NextCursor != after {
				return remotehttp.ErrInvalidResponse
			}
			return nil
		}
		_, err = validatePullChanges(after, page)
		if err != nil {
			return err
		}
		if err := store.IngestPullPage(ctx, after, page.NextCursor, page.Changes); err != nil {
			return err
		}
		if testHookAfterInboxIngest != nil {
			if err := testHookAfterInboxIngest(); err != nil {
				return err
			}
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
		if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
			return err
		}
		if err := c.publishStagedConflicts(ctx, store, remote); err != nil {
			return err
		}
		if err := c.cleanupCompletedOutboxBlobs(ctx, store); err != nil {
			return err
		}
		if !page.HasMore {
			return nil
		}
	}
	return errors.New("foreground pull page bound exceeded")
}

func validatePullChanges(after uint64, page remotehttp.PullPage) (uint64, error) {
	expected := after
	for _, change := range page.Changes {
		expected++
		if change.Cursor != expected {
			return 0, remotehttp.ErrInvalidResponse
		}
		if change.ObjectType == clientsync.Folder {
			validMutation := change.Mutation == clientsync.Create || change.Mutation == clientsync.Move || change.Mutation == clientsync.Delete
			validRevision := change.Mutation == clientsync.Create && change.Revision == 1 || change.Mutation != clientsync.Create && change.Revision >= 2
			if !validMutation || !validRevision || change.Deleted != (change.Mutation == clientsync.Delete) || len(change.BlobHash) != 0 {
				return 0, ErrUnsupportedPullPage
			}
		} else if change.ObjectType != clientsync.Note || (change.Mutation != clientsync.Create && change.Mutation != clientsync.Update && change.Mutation != clientsync.Move && change.Mutation != clientsync.Delete) {
			return 0, ErrUnsupportedPullPage
		}
	}
	if page.NextCursor != expected || page.NextCursor <= after {
		return 0, remotehttp.ErrInvalidResponse
	}
	return expected, nil
}

func ingestPreserveDeleteProbeHistory(ctx context.Context, store *clientsync.Store, remote SyncRemote, blocked map[uuid.UUID]uint64) (bool, error) {
	type held struct {
		after uint64
		page  remotehttp.PullPage
	}
	var pages []held
	after, err := store.DownloadedCursor(ctx)
	if err != nil {
		return false, err
	}
	matched := false
	for pageNumber := 0; pageNumber < maxForegroundPullPages; pageNumber++ {
		page, err := remote.Pull(ctx, after, 100)
		if err != nil {
			return false, err
		}
		if len(page.Changes) == 0 {
			if page.HasMore || page.NextCursor != after {
				return false, remotehttp.ErrInvalidResponse
			}
			if !matched {
				return false, nil
			}
			for _, h := range pages {
				if err = store.IngestPullPage(ctx, h.after, h.page.NextCursor, h.page.Changes); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		if _, err = validatePullChanges(after, page); err != nil {
			return false, err
		}
		for _, change := range page.Changes {
			if base, ok := blocked[change.ObjectID]; ok && change.ObjectType == clientsync.Folder && change.Mutation == clientsync.Move && change.Revision > base {
				matched = true
			}
		}
		pages = append(pages, held{after, page})
		after = page.NextCursor
		if !page.HasMore {
			if !matched {
				return false, nil
			}
			for _, h := range pages {
				if err = store.IngestPullPage(ctx, h.after, h.page.NextCursor, h.page.Changes); err != nil {
					return false, err
				}
			}
			return true, nil
		}
	}
	if matched {
		return false, errors.New("preserve delete history page bound exceeded")
	}
	return false, nil
}

// ingestPullWhileOutboundBlocked only extends the durable downloaded frontier.
// It intentionally creates no cursor-ordered legacy plan.
func (c *LocalCore) ingestPullWhileOutboundBlocked(ctx context.Context, store *clientsync.Store, remote SyncRemote) error {
	for pageNumber := 0; pageNumber < maxForegroundPullPages; pageNumber++ {
		after, err := store.DownloadedCursor(ctx)
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
		if _, err := validatePullChanges(after, page); err != nil {
			return err
		}
		if err := store.IngestPullPage(ctx, after, page.NextCursor, page.Changes); err != nil {
			return err
		}
		if testHookAfterInboxIngest != nil {
			if err := testHookAfterInboxIngest(); err != nil {
				return err
			}
		}
		if !page.HasMore {
			return nil
		}
	}
	return nil
}

func (c *LocalCore) drainIndependentInbox(ctx context.Context, store *clientsync.Store, remote SyncRemote) error {
	applied := 0
	for applied < maxIndependentInboxApplies {
		candidates, err := store.ListIndependentInboxCandidates(ctx, maxIndependentInboxScan)
		if err != nil {
			return err
		}
		progressed := false
		for _, candidate := range candidates {
			if applied >= maxIndependentInboxApplies {
				break
			}
			unresolved, err := store.HasUnresolvedLocalIntent(ctx, candidate.Change.ObjectID)
			if err != nil {
				return err
			}
			if unresolved {
				continue
			}
			planID, err := uuid.NewV7()
			if err != nil {
				return fmt.Errorf("generate independent apply plan id: %w", err)
			}
			if err := store.CreateInboxApplyPlan(ctx, candidate.Change.Cursor, planID); err != nil {
				return err
			}
			if err := c.executeActiveApplyPlanLocked(ctx, remote); err != nil {
				return err
			}
			if err := store.ReconcileInboxAppliedThroughConfirmed(ctx); err != nil {
				return err
			}
			if err := c.publishStagedConflicts(ctx, store, remote); err != nil {
				return err
			}
			if err := c.cleanupCompletedOutboxBlobs(ctx, store); err != nil {
				return err
			}
			applied++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return nil
}
