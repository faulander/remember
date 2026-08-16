package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/frontmatter"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
	"github.com/faulander/remember/client/internal/repository"
	"github.com/google/uuid"
)

func divergentFolderTreeCandidates(root localindex.Object, snapshot localindex.Snapshot) ([]clientsync.DivergentFolderTreeCandidate, error) {
	prefix := root.RelativePath + "/"
	candidates := make([]clientsync.DivergentFolderTreeCandidate, 0)
	for _, object := range snapshot.Objects {
		if object.ID == root.ID || !strings.HasPrefix(object.RelativePath, prefix) {
			continue
		}
		relative := strings.TrimPrefix(object.RelativePath, prefix)
		candidate := clientsync.DivergentFolderTreeCandidate{ObjectID: object.ID, ParentID: object.ParentID, Name: path.Base(relative), RelativePath: relative, Depth: 1 + strings.Count(relative, "/"), Device: object.FolderDevice, Inode: object.FolderInode}
		switch object.Type {
		case localindex.ObjectFolder:
			candidate.ObjectType = clientsync.Folder
		case localindex.ObjectNote:
			candidate.ObjectType = clientsync.Note
			if len(object.ContentHash) != sha256.Size {
				return nil, clientsync.ErrDivergentFolderMoveIneligible
			}
			copy(candidate.BlobHash[:], object.ContentHash)
		default:
			return nil, clientsync.ErrDivergentFolderMoveIneligible
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) > clientsync.MaxDivergentFolderTreeMembers {
		return nil, clientsync.ErrDivergentFolderMoveIneligible
	}
	sortDivergentTreeCandidates(candidates)
	return candidates, nil
}

var errDivergentTreeAwaitingPull = errors.New("divergent tree recovery awaiting authenticated pull")

func sortDivergentTreeCandidates(candidates []clientsync.DivergentFolderTreeCandidate) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Depth != candidates[j].Depth {
			return candidates[i].Depth < candidates[j].Depth
		}
		return candidates[i].RelativePath < candidates[j].RelativePath
	})
}

func prepareDivergentFolderTreeManifest(root, sourceRelative string, sourceDevice, sourceInode uint64, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	if manifest == nil {
		return clientsync.ErrDivergentFolderMoveIneligible
	}
	for i := range manifest.Members {
		member := &manifest.Members[i]
		if member.SourceRevision == 0 || member.ObjectType != clientsync.Note {
			continue
		}
		content, err := repository.ReadRooted(root, path.Join(sourceRelative, member.RelativePath), clientsync.MaxBlobBytes)
		if err != nil || sha256.Sum256(content) != member.SourceBlobHash {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		inspection, err := frontmatter.Inspect(content)
		if err != nil || inspection.NoteID != member.SourceObjectID {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		rekeyed, err := frontmatter.RekeyIdentity(content, member.SourceObjectID, member.RecoveredObjectID)
		if err != nil {
			return clientsync.ErrDivergentFolderMoveIneligible
		}
		member.RecoveredBlobHash = sha256.Sum256(rekeyed)
	}
	return verifyDivergentFolderTreeSource(root, sourceRelative, sourceDevice, sourceInode, manifest)
}

func divergentTreeEntries(manifest *clientsync.ConflictFolderDivergentTreeManifest, recovered bool, knownOnly bool) []repository.RootedSubtreeEntry {
	entries := make([]repository.RootedSubtreeEntry, 0, len(manifest.Members))
	for _, member := range manifest.Members {
		if knownOnly && member.SourceRevision == 0 {
			continue
		}
		entry := repository.RootedSubtreeEntry{Relative: member.RelativePath}
		if member.ObjectType == clientsync.Folder {
			entry.Kind = repository.RootedSubtreeFolder
			if knownOnly {
				entry.Device, entry.Inode = member.CanonicalDevice, member.CanonicalInode
			} else {
				entry.Device, entry.Inode = member.Device, member.Inode
			}
		} else {
			entry.Kind = repository.RootedSubtreeFile
			entry.Hash = member.SourceBlobHash
			if recovered {
				entry.Hash = member.RecoveredBlobHash
			}
		}
		entries = append(entries, entry)
	}
	return entries
}

func verifyDivergentFolderTreeSource(root, relative string, device, inode uint64, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, divergentTreeEntries(manifest, false, false), clientsync.MaxBlobBytes)
}

func verifyDivergentFolderTreeRecovery(root, relative string, device, inode uint64, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, divergentTreeEntries(manifest, true, false), clientsync.MaxBlobBytes)
}

func verifyDivergentFolderTreeTransition(root, relative string, device, inode uint64, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	entries := divergentTreeEntries(manifest, false, false)
	for i := range entries {
		member := manifest.Members[i]
		if member.ObjectType != clientsync.Note || member.SourceRevision == 0 {
			continue
		}
		content, err := repository.ReadRooted(root, path.Join(relative, member.RelativePath), clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(content)
		if hash != member.SourceBlobHash && hash != member.RecoveredBlobHash {
			return errors.New("divergent tree note transition hash mismatch")
		}
		entries[i].Hash = hash
	}
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, entries, clientsync.MaxBlobBytes)
}

func ensureDivergentCanonicalStage(root, stage, sourceRelative string, nonce [sha256.Size]byte, manifest *clientsync.ConflictFolderDivergentTreeManifest) (uint64, uint64, map[uuid.UUID][2]uint64, error) {
	device, inode, err := repository.RootedFolderIdentity(root, stage)
	if err == nil {
		if err := repository.VerifyRootedFolderPublication(root, stage, nonce, device, inode); err != nil {
			return 0, 0, nil, err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		device, inode, err = repository.CreateRootedFolderPublication(root, stage, nonce)
		if err != nil {
			return 0, 0, nil, err
		}
	} else {
		return 0, 0, nil, err
	}
	folders := map[uuid.UUID][2]uint64{}
	for _, member := range manifest.Members {
		if member.SourceRevision == 0 {
			continue
		}
		target := path.Join(stage, member.RelativePath)
		if member.ObjectType == clientsync.Folder {
			folderDevice, folderInode, folderErr := repository.RootedFolderIdentity(root, target)
			if errors.Is(folderErr, os.ErrNotExist) {
				if err := repository.CreateRootedDirectory(root, target, 0o700); err != nil {
					return 0, 0, nil, err
				}
				folderDevice, folderInode, folderErr = repository.RootedFolderIdentity(root, target)
			}
			if folderErr != nil {
				return 0, 0, nil, folderErr
			}
			folders[member.SourceObjectID] = [2]uint64{folderDevice, folderInode}
			continue
		}
		staged, stagedErr := repository.ReadRooted(root, target, clientsync.MaxBlobBytes)
		if stagedErr == nil {
			if sha256.Sum256(staged) != member.SourceBlobHash || noteID(staged) != member.SourceObjectID {
				return 0, 0, nil, errors.New("divergent canonical staged note mismatch")
			}
			continue
		}
		if !errors.Is(stagedErr, os.ErrNotExist) {
			return 0, 0, nil, stagedErr
		}
		source, err := repository.ReadRooted(root, path.Join(sourceRelative, member.RelativePath), clientsync.MaxBlobBytes)
		if err != nil || sha256.Sum256(source) != member.SourceBlobHash || noteID(source) != member.SourceObjectID {
			return 0, 0, nil, errors.New("divergent canonical source note unavailable")
		}
		if err := repository.CreateRooted(root, target, source, validateNoteID(member.SourceObjectID)); err != nil {
			return 0, 0, nil, err
		}
	}
	entries := canonicalStageEntries(manifest, folders, nonce, true)
	if err := repository.VerifyRootedSubtreeExpected(root, stage, device, inode, entries, clientsync.MaxBlobBytes); err != nil {
		return 0, 0, nil, err
	}
	return device, inode, folders, nil
}

func canonicalStageEntries(manifest *clientsync.ConflictFolderDivergentTreeManifest, folders map[uuid.UUID][2]uint64, nonce [sha256.Size]byte, marker bool) []repository.RootedSubtreeEntry {
	entries := make([]repository.RootedSubtreeEntry, 0, len(manifest.Members)+1)
	if marker {
		entries = append(entries, repository.RootedSubtreeEntry{Relative: ".remember-apply-nonce", Kind: repository.RootedSubtreeFile, Hash: sha256.Sum256(nonce[:])})
	}
	for _, member := range manifest.Members {
		if member.SourceRevision == 0 {
			continue
		}
		entry := repository.RootedSubtreeEntry{Relative: member.RelativePath}
		if member.ObjectType == clientsync.Folder {
			identity := folders[member.SourceObjectID]
			entry.Kind, entry.Device, entry.Inode = repository.RootedSubtreeFolder, identity[0], identity[1]
		} else {
			entry.Kind, entry.Hash = repository.RootedSubtreeFile, member.SourceBlobHash
		}
		entries = append(entries, entry)
	}
	return entries
}

func verifyDivergentCanonicalTree(root, relative string, device, inode uint64, manifest *clientsync.ConflictFolderDivergentTreeManifest, marker bool, nonce [sha256.Size]byte) error {
	folders := map[uuid.UUID][2]uint64{}
	for _, member := range manifest.Members {
		if member.SourceRevision > 0 && member.ObjectType == clientsync.Folder {
			if member.CanonicalDevice == 0 || member.CanonicalInode == 0 {
				return errors.New("missing divergent canonical folder binding")
			}
			folders[member.SourceObjectID] = [2]uint64{member.CanonicalDevice, member.CanonicalInode}
		}
	}
	return repository.VerifyRootedSubtreeExpected(root, relative, device, inode, canonicalStageEntries(manifest, folders, nonce, marker), clientsync.MaxBlobBytes)
}

func noteID(content []byte) uuid.UUID {
	inspection, err := frontmatter.Inspect(content)
	if err != nil {
		return uuid.Nil
	}
	return inspection.NoteID
}

func validateNoteID(expected uuid.UUID) repository.Validator {
	return func(content []byte) error {
		if noteID(content) != expected {
			return errors.New("note identity mismatch")
		}
		return nil
	}
}

func rewriteDivergentTreeRecoveryNotes(root, recoveryRelative string, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	for _, member := range manifest.Members {
		if member.SourceRevision == 0 || member.ObjectType != clientsync.Note {
			continue
		}
		relative := path.Join(recoveryRelative, member.RelativePath)
		content, err := repository.ReadRooted(root, relative, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(content)
		if hash == member.RecoveredBlobHash && noteID(content) == member.RecoveredObjectID {
			continue
		}
		if hash != member.SourceBlobHash || noteID(content) != member.SourceObjectID {
			return errors.New("divergent recovery note changed")
		}
		rekeyed, err := frontmatter.RekeyIdentity(content, member.SourceObjectID, member.RecoveredObjectID)
		if err != nil || sha256.Sum256(rekeyed) != member.RecoveredBlobHash {
			return errors.New("divergent recovery note rekey mismatch")
		}
		if err := repository.WriteRootedExpected(root, relative, content, rekeyed, validateNoteID(member.RecoveredObjectID)); err != nil {
			return err
		}
	}
	return nil
}

func restoreDivergentTreeSourceNotes(root, stage, recoveryRelative string, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	for _, member := range manifest.Members {
		if member.SourceRevision == 0 || member.ObjectType != clientsync.Note {
			continue
		}
		relative := path.Join(recoveryRelative, member.RelativePath)
		current, err := repository.ReadRooted(root, relative, clientsync.MaxBlobBytes)
		if err != nil {
			return err
		}
		if sha256.Sum256(current) == member.SourceBlobHash && noteID(current) == member.SourceObjectID {
			continue
		}
		if sha256.Sum256(current) != member.RecoveredBlobHash || noteID(current) != member.RecoveredObjectID {
			return errors.New("divergent recovery rollback note changed")
		}
		original, err := repository.ReadRooted(root, path.Join(stage, member.RelativePath), clientsync.MaxBlobBytes)
		if err != nil || sha256.Sum256(original) != member.SourceBlobHash || noteID(original) != member.SourceObjectID {
			return errors.New("divergent canonical rollback source unavailable")
		}
		if err := repository.WriteRootedExpected(root, relative, current, original, validateNoteID(member.SourceObjectID)); err != nil {
			return err
		}
	}
	return nil
}

func divergentTreeReconcileData(manifest *clientsync.ConflictFolderDivergentTreeManifest, target string, rootID uuid.UUID, mode string) recursiveLocalReconcileData {
	data := recursiveLocalReconcileData{notes: map[uuid.UUID][32]byte{}, notePaths: map[uuid.UUID]string{}, folders: map[uuid.UUID]bool{rootID: true}, folderPaths: map[uuid.UUID]string{rootID: target}, trusted: map[string]uuid.UUID{target: rootID}}
	for _, member := range manifest.Members {
		if mode == "canonical" && member.SourceRevision == 0 {
			continue
		}
		id := member.SourceObjectID
		hash := member.SourceBlobHash
		if mode == "recovery" {
			id, hash = member.RecoveredObjectID, member.RecoveredBlobHash
		}
		relative := path.Join(target, member.RelativePath)
		if member.ObjectType == clientsync.Folder {
			data.folders[id] = true
			data.folderPaths[id] = relative
			data.trusted[relative] = id
		} else {
			data.notes[id] = hash
			data.notePaths[id] = relative
		}
	}
	return data
}

func mergeDivergentTreeReconcileData(left, right recursiveLocalReconcileData) recursiveLocalReconcileData {
	for id, value := range right.notes {
		left.notes[id] = value
	}
	for id, value := range right.notePaths {
		left.notePaths[id] = value
	}
	for id, value := range right.folders {
		left.folders[id] = value
	}
	for id, value := range right.folderPaths {
		left.folderPaths[id] = value
	}
	for relative, id := range right.trusted {
		left.trusted[relative] = id
	}
	return left
}

func (c *LocalCore) recoverDivergentFolderMoveTree(ctx context.Context, store *clientsync.Store, recovery *clientsync.ConflictFolderDivergentMoveRecovery, manifest *clientsync.ConflictFolderDivergentTreeManifest) error {
	stage := ".remember/conflicts/folders/" + recovery.OperationID.String()
	recoveryData := divergentTreeReconcileData(manifest, recovery.RecoveryRelative, recovery.RecoveredFolderID, "recovery")
	attemptedData := divergentTreeReconcileData(manifest, recovery.AttemptedRelative, recovery.FolderID, "source")
	restorePrepared := func(cause error) error {
		if _, _, _, err := ensureDivergentCanonicalStage(c.root, stage, recovery.RecoveryRelative, recovery.CanonicalNonce, manifest); err != nil {
			return errors.Join(cause, err)
		}
		if err := restoreDivergentTreeSourceNotes(c.root, stage, recovery.RecoveryRelative, manifest); err != nil {
			return errors.Join(cause, err)
		}
		if err := verifyDivergentFolderTreeSource(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
			return errors.Join(cause, err)
		}
		if err := repository.MoveRootedFolderExpected(c.root, recovery.RecoveryRelative, recovery.AttemptedRelative, recovery.SourceDevice, recovery.SourceInode); err != nil {
			return errors.Join(cause, err)
		}
		_, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: attemptedData.notes, AppliedRemoteNotePaths: attemptedData.notePaths, AppliedRemoteFolders: attemptedData.folders, AppliedRemoteFolderPaths: attemptedData.folderPaths, TrustedRemoteFolders: attemptedData.trusted, VerifyTrustedRemoteFolders: func() error {
			return verifyDivergentFolderTreeSource(c.root, recovery.AttemptedRelative, recovery.SourceDevice, recovery.SourceInode, manifest)
		}})
		return errors.Join(cause, err)
	}
	if recovery.State == "prepared" {
		if err := store.ValidateConflictFolderDivergentTreeManifest(ctx, recovery.FolderID, *manifest); err != nil {
			return fmt.Errorf("validate divergent folder tree manifest: %w", err)
		}
		sourceRelative := recovery.AttemptedRelative
		if err := verifyDivergentFolderTreeTransition(c.root, sourceRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
			sourceRelative = recovery.RecoveryRelative
			if err := verifyDivergentFolderTreeTransition(c.root, sourceRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
				return err
			}
		}
		if _, _, _, err := ensureDivergentCanonicalStage(c.root, stage, sourceRelative, recovery.CanonicalNonce, manifest); err != nil {
			return err
		}
		if sourceRelative == recovery.AttemptedRelative {
			if err := repository.MoveRootedFolderExpected(c.root, recovery.AttemptedRelative, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode); err != nil {
				return err
			}
		}
		if testHookAfterDivergentFolderEvacuationMove != nil {
			if err := testHookAfterDivergentFolderEvacuationMove(); err != nil {
				return restorePrepared(err)
			}
		}
		if err := rewriteDivergentTreeRecoveryNotes(c.root, recovery.RecoveryRelative, manifest); err != nil {
			return restorePrepared(err)
		}
		if testHookAfterDivergentTreeRewrite != nil {
			if err := testHookAfterDivergentTreeRewrite(); err != nil {
				return restorePrepared(err)
			}
		}
		verify := func() error {
			return verifyDivergentFolderTreeRecovery(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest)
		}
		if err := verify(); err != nil {
			return restorePrepared(err)
		}
		removedCanonical := map[uuid.UUID]bool{recovery.FolderID: true}
		for _, member := range manifest.Members {
			if member.SourceRevision > 0 {
				removedCanonical[member.SourceObjectID] = true
			}
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteDeletes: removedCanonical, AppliedRemoteNotes: recoveryData.notes, AppliedRemoteNotePaths: recoveryData.notePaths, AppliedRemoteFolders: recoveryData.folders, AppliedRemoteFolderPaths: recoveryData.folderPaths, TrustedRemoteFolderDeletes: map[string]uuid.UUID{recovery.AttemptedRelative: recovery.FolderID}, TrustedRemoteFolders: recoveryData.trusted, VerifyTrustedRemoteFolders: verify}); err != nil {
			return restorePrepared(fmt.Errorf("reconcile divergent tree evacuation: %w", err))
		}
		if testHookAfterDivergentFolderEvacuationReconcile != nil {
			if err := testHookAfterDivergentFolderEvacuationReconcile(); err != nil {
				return restorePrepared(err)
			}
		}
		if testHookBeforeDivergentFolderEvacuatedTransition != nil {
			if err := testHookBeforeDivergentFolderEvacuatedTransition(); err != nil {
				return restorePrepared(err)
			}
		}
		if err := store.MarkConflictFolderDivergentMoveEvacuated(ctx, recovery.OperationID); err != nil {
			return restorePrepared(fmt.Errorf("seal divergent tree evacuation: %w", err))
		}
		recovery.State = "evacuated"
	}
	if recovery.State == "evacuated" {
		if err := verifyDivergentFolderTreeRecovery(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
			return err
		}
		device, inode, folders, err := ensureDivergentCanonicalStage(c.root, stage, recovery.RecoveryRelative, recovery.CanonicalNonce, manifest)
		if err != nil {
			return err
		}
		if testHookAfterDivergentCanonicalStageCreate != nil {
			if err := testHookAfterDivergentCanonicalStageCreate(); err != nil {
				return err
			}
		}
		if err := store.MarkConflictFolderDivergentMoveCanonicalPreparedWithTree(ctx, recovery.OperationID, device, inode, folders); err != nil {
			return err
		}
		recovery.State, recovery.CanonicalDevice, recovery.CanonicalInode = "canonical_prepared", device, inode
		loaded, err := store.ConflictFolderDivergentTreeManifest(ctx, recovery.OperationID)
		if err != nil {
			return err
		}
		manifest = loaded
	}
	if recovery.State == "canonical_prepared" {
		if err := repository.VerifyRootedFolderPublication(c.root, stage, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err == nil {
			if err := verifyDivergentCanonicalTree(c.root, stage, recovery.CanonicalDevice, recovery.CanonicalInode, manifest, true, recovery.CanonicalNonce); err != nil {
				return err
			}
			if err := repository.PublishRootedFolderPublication(c.root, stage, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err != nil {
				return err
			}
		} else {
			if err := repository.VerifyRootedFolderPublication(c.root, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err != nil {
				return err
			}
		}
		if testHookAfterDivergentCanonicalPublish != nil {
			if err := testHookAfterDivergentCanonicalPublish(); err != nil {
				return err
			}
		}
		if err := store.MarkConflictFolderDivergentMoveCanonicalPublished(ctx, recovery.OperationID); err != nil {
			return err
		}
		recovery.State = "canonical_published"
	}
	if recovery.State == "canonical_published" {
		if err := repository.CleanupRootedFolderPublication(c.root, recovery.CanonicalRelative, recovery.CanonicalNonce, recovery.CanonicalDevice, recovery.CanonicalInode); err != nil {
			return err
		}
		if testHookAfterDivergentCanonicalCleanup != nil {
			if err := testHookAfterDivergentCanonicalCleanup(); err != nil {
				return err
			}
		}
		if err := verifyDivergentFolderTreeRecovery(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
			return err
		}
		if err := verifyDivergentCanonicalTree(c.root, recovery.CanonicalRelative, recovery.CanonicalDevice, recovery.CanonicalInode, manifest, false, recovery.CanonicalNonce); err != nil {
			return err
		}
		canonicalData := divergentTreeReconcileData(manifest, recovery.CanonicalRelative, recovery.FolderID, "canonical")
		finalData := mergeDivergentTreeReconcileData(recoveryData, canonicalData)
		verify := func() error {
			if err := verifyDivergentFolderTreeRecovery(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest); err != nil {
				return err
			}
			return verifyDivergentCanonicalTree(c.root, recovery.CanonicalRelative, recovery.CanonicalDevice, recovery.CanonicalInode, manifest, false, recovery.CanonicalNonce)
		}
		if _, err := reconcile.Run(ctx, c.root, c.index, reconcile.Options{RecoveryMode: c.recoveryMode, AppliedRemoteNotes: finalData.notes, AppliedRemoteNotePaths: finalData.notePaths, AppliedRemoteFolders: finalData.folders, AppliedRemoteFolderPaths: finalData.folderPaths, TrustedRemoteFolders: finalData.trusted, VerifyTrustedRemoteFolders: verify}); err != nil {
			return fmt.Errorf("reconcile divergent tree publication: %w", err)
		}
		for _, member := range manifest.Members {
			if member.ObjectType == clientsync.Note {
				if err := clientsync.StageNote(c.root, path.Join(recovery.RecoveryRelative, member.RelativePath), member.RecoveredBlobHash); err != nil {
					return err
				}
			}
		}
		revision, found, err := store.Baseline(ctx, recovery.FolderID)
		if err != nil {
			return err
		}
		if !found || revision != recovery.CanonicalRevision {
			return errDivergentTreeAwaitingPull
		}
		if _, found, err := store.Baseline(ctx, clientsync.ConflictRecoveredID); err != nil {
			return err
		} else if !found {
			return errDivergentTreeAwaitingPull
		}
		if err := store.CompleteConflictFolderDivergentMoveRecovery(ctx, *recovery); err != nil {
			return err
		}
		recovery.State = "completed"
	}
	return verifyDivergentFolderTreeRecovery(c.root, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, manifest)
}
