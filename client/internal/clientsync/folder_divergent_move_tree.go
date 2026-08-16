package clientsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

const (
	MaxDivergentFolderTreeDepth   = 256
	MaxDivergentFolderTreeMembers = 10000
)

type DivergentFolderTreeCandidate struct {
	ObjectID, ParentID uuid.UUID
	ObjectType         ObjectType
	Name, RelativePath string
	Depth              int
	Device, Inode      uint64
	BlobHash           [sha256.Size]byte
}

type ConflictFolderDivergentTreeMember struct {
	SourceObjectID, RecoveredObjectID                 uuid.UUID
	SourceParentID, RecoveredParentID                 uuid.UUID
	SourceOperationID, NewOperationID                 uuid.UUID
	ObjectType                                        ObjectType
	Name, RelativePath                                string
	Depth                                             int
	SourceRevision                                    uint64
	Device, Inode                                     uint64
	SourceBlobHash, RecoveredBlobHash, CreateBlobHash [sha256.Size]byte
	CanonicalDevice, CanonicalInode                   uint64
	Chain                                             []ConflictFolderCreateNoteChainOperation
}

type ConflictFolderDivergentTreeManifest struct {
	OperationID, NewRootOperationID uuid.UUID
	Members                         []ConflictFolderDivergentTreeMember
}

func validDivergentFolderTreeCandidate(candidate DivergentFolderTreeCandidate) bool {
	if !validObjectID(candidate.ObjectID) || !validObjectID(candidate.ParentID) || candidate.ObjectID == candidate.ParentID || candidate.Depth < 1 || candidate.Depth > MaxDivergentFolderTreeDepth || naming.ValidateComponent(candidate.Name) != nil || naming.ValidateRelativePath(candidate.RelativePath) != nil || path.Base(candidate.RelativePath) != candidate.Name {
		return false
	}
	zero := [sha256.Size]byte{}
	switch candidate.ObjectType {
	case Folder:
		return candidate.Device > 0 && candidate.Inode > 0 && candidate.Device <= math.MaxInt64 && candidate.Inode <= math.MaxInt64 && candidate.BlobHash == zero
	case Note:
		return candidate.Device == 0 && candidate.Inode == 0 && candidate.BlobHash != zero
	default:
		return false
	}
}

func validDivergentFolderTreeMember(member ConflictFolderDivergentTreeMember) bool {
	candidate := DivergentFolderTreeCandidate{ObjectID: member.SourceObjectID, ParentID: member.SourceParentID, ObjectType: member.ObjectType, Name: member.Name, RelativePath: member.RelativePath, Depth: member.Depth, Device: member.Device, Inode: member.Inode, BlobHash: member.SourceBlobHash}
	if !validDivergentFolderTreeCandidate(candidate) || !validObjectID(member.RecoveredObjectID) || !validObjectID(member.RecoveredParentID) || !validOperationID(member.SourceOperationID) || !validOperationID(member.NewOperationID) || member.SourceOperationID == member.NewOperationID || member.SourceRevision > math.MaxInt64 {
		return false
	}
	zero := [sha256.Size]byte{}
	if member.SourceRevision == 0 {
		if member.RecoveredObjectID != member.SourceObjectID || member.ObjectType == Folder && (member.CreateBlobHash != zero || member.RecoveredBlobHash != zero) || member.ObjectType == Note && (member.CreateBlobHash == zero || member.RecoveredBlobHash != member.SourceBlobHash) {
			return false
		}
	} else if member.RecoveredObjectID == member.SourceObjectID || member.CreateBlobHash != zero || len(member.Chain) != 0 || member.ObjectType == Folder && member.RecoveredBlobHash != zero || member.ObjectType == Note && member.RecoveredBlobHash == zero {
		return false
	}
	if member.ObjectType == Folder {
		if member.CanonicalDevice > math.MaxInt64 || member.CanonicalInode > math.MaxInt64 || (member.CanonicalDevice == 0) != (member.CanonicalInode == 0) {
			return false
		}
	} else if member.CanonicalDevice != 0 || member.CanonicalInode != 0 {
		return false
	}
	return true
}

func (s *Store) PlanConflictFolderDivergentTree(ctx context.Context, operationID, rootID, recoveredRootID, newRootOperationID uuid.UUID, candidates []DivergentFolderTreeCandidate) (*ConflictFolderDivergentTreeManifest, error) {
	if !validOperationID(operationID) || !validObjectID(rootID) || !validObjectID(recoveredRootID) || rootID == recoveredRootID || !validOperationID(newRootOperationID) || operationID == newRootOperationID || len(candidates) == 0 || len(candidates) > MaxDivergentFolderTreeMembers {
		return nil, errors.New("invalid divergent folder tree plan")
	}
	var result *ConflictFolderDivergentTreeManifest
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var rootValid int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox WHERE operation_id=? AND object_id=? AND mutation='move' AND object_type='folder' AND status='conflict' AND conflict_code='base_revision_mismatch')`, operationID.String(), rootID.String()).Scan(&rootValid); err != nil {
			return err
		}
		if rootValid != 1 {
			return ErrDivergentFolderMoveIneligible
		}
		manifest := &ConflictFolderDivergentTreeManifest{OperationID: operationID, NewRootOperationID: newRootOperationID}
		seenSources := map[uuid.UUID]bool{}
		seenRecovered := map[uuid.UUID]bool{recoveredRootID: true}
		sourceOperations := map[uuid.UUID]uuid.UUID{rootID: operationID}
		recoveredIDs := map[uuid.UUID]uuid.UUID{rootID: recoveredRootID}
		relatives := map[uuid.UUID]string{rootID: ""}
		allowedChildren := map[uuid.UUID]map[uuid.UUID]bool{}
		known := 0
		var err error
		for _, candidate := range candidates {
			if !validDivergentFolderTreeCandidate(candidate) || seenSources[candidate.ObjectID] || !seenSources[candidate.ParentID] && candidate.ParentID != rootID {
				return ErrDivergentFolderMoveIneligible
			}
			expectedRelative := candidate.Name
			if parentRelative := relatives[candidate.ParentID]; parentRelative != "" {
				expectedRelative = path.Join(parentRelative, candidate.Name)
			}
			if candidate.RelativePath != expectedRelative || candidate.Depth != 1+pathDepth(parentRelative(relatives, candidate.ParentID)) {
				return ErrDivergentFolderMoveIneligible
			}
			member := ConflictFolderDivergentTreeMember{SourceObjectID: candidate.ObjectID, SourceParentID: candidate.ParentID, RecoveredParentID: recoveredIDs[candidate.ParentID], ObjectType: candidate.ObjectType, Name: candidate.Name, RelativePath: candidate.RelativePath, Depth: candidate.Depth, Device: candidate.Device, Inode: candidate.Inode, SourceBlobHash: candidate.BlobHash}
			var baselineOperation sql.NullString
			if err := tx.QueryRowContext(ctx, `SELECT revision,operation_id FROM sync_baselines WHERE object_id=?`, candidate.ObjectID.String()).Scan(&member.SourceRevision, &baselineOperation); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			} else if err == nil {
				if !baselineOperation.Valid {
					return ErrDivergentFolderMoveIneligible
				}
				member.SourceOperationID, err = uuid.Parse(baselineOperation.String)
				if err != nil || !validOperationID(member.SourceOperationID) {
					return errors.New("corrupt divergent descendant baseline")
				}
				var open int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=? AND status IN('pending','attempted','replay_mismatch','conflict')`, candidate.ObjectID.String()).Scan(&open); err != nil {
					return err
				}
				if open != 0 {
					return ErrDivergentFolderMoveIneligible
				}
				member.RecoveredObjectID, err = uuid.NewV7()
				if err != nil {
					return err
				}
				known++
			} else {
				if err := planLocalDivergentTreeMemberTx(ctx, tx, &member, sourceOperations[candidate.ParentID], allowedChildren); err != nil {
					return err
				}
				member.RecoveredObjectID = member.SourceObjectID
				member.RecoveredBlobHash = member.SourceBlobHash
			}
			member.NewOperationID, err = uuid.NewV7()
			if err != nil {
				return err
			}
			if seenRecovered[member.RecoveredObjectID] {
				return ErrDivergentFolderMoveIneligible
			}
			seenSources[member.SourceObjectID] = true
			seenRecovered[member.RecoveredObjectID] = true
			sourceOperations[member.SourceObjectID] = member.SourceOperationID
			recoveredIDs[member.SourceObjectID] = member.RecoveredObjectID
			relatives[member.SourceObjectID] = member.RelativePath
			manifest.Members = append(manifest.Members, member)
		}
		if known == 0 {
			return nil
		}
		if err := validateDivergentTreeDependencyClosureTx(ctx, tx, manifest, allowedChildren); err != nil {
			return err
		}
		result = manifest
		return nil
	})
	return result, err
}

func parentRelative(relatives map[uuid.UUID]string, id uuid.UUID) string { return relatives[id] }
func pathDepth(relative string) int {
	if relative == "" {
		return 0
	}
	return 1 + bytes.Count([]byte(relative), []byte{'/'})
}

func planLocalDivergentTreeMemberTx(ctx context.Context, tx *sql.Tx, member *ConflictFolderDivergentTreeMember, expectedParentOperation uuid.UUID, allowedChildren map[uuid.UUID]map[uuid.UUID]bool) error {
	var operation, objectType, name string
	var parent sql.NullString
	var blob []byte
	var attempted sql.NullInt64
	rows, err := tx.QueryContext(ctx, `SELECT operation_id,object_type,parent_id,name,blob_hash,attempted_at_ms FROM sync_outbox WHERE object_id=? ORDER BY sequence`, member.SourceObjectID.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	type historyRow struct {
		operation, objectType, name string
		parent                      sql.NullString
		blob                        []byte
		attempted                   sql.NullInt64
	}
	var history []historyRow
	for rows.Next() {
		if err := rows.Scan(&operation, &objectType, &parent, &name, &blob, &attempted); err != nil {
			return err
		}
		history = append(history, historyRow{operation: operation, objectType: objectType, parent: parent, name: name, blob: append([]byte(nil), blob...), attempted: attempted})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(history) == 0 {
		return ErrDivergentFolderMoveIneligible
	}
	create := history[0]
	member.SourceOperationID, err = uuid.Parse(create.operation)
	if err != nil || !validOperationID(member.SourceOperationID) || create.objectType != string(member.ObjectType) || !create.parent.Valid || create.parent.String != member.SourceParentID.String() || create.name != member.Name || create.attempted.Valid {
		return ErrDivergentFolderMoveIneligible
	}
	var exactCreate int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox WHERE operation_id=? AND mutation='create' AND status='pending')`, create.operation).Scan(&exactCreate); err != nil || exactCreate != 1 {
		return ErrDivergentFolderMoveIneligible
	}
	if err := requireSingleDependencyTx(ctx, tx, member.SourceOperationID, expectedParentOperation); err != nil {
		return err
	}
	allowDependency(allowedChildren, expectedParentOperation, member.SourceOperationID)
	zero := [sha256.Size]byte{}
	if member.ObjectType == Folder {
		if create.blob != nil || len(history) != 1 {
			return ErrDivergentFolderMoveIneligible
		}
		member.CreateBlobHash = zero
		return nil
	}
	if len(create.blob) != sha256.Size {
		return ErrDivergentFolderMoveIneligible
	}
	copy(member.CreateBlobHash[:], create.blob)
	previous := member.SourceOperationID
	finalHash := member.CreateBlobHash
	for _, update := range history[1:] {
		updateID, parseErr := uuid.Parse(update.operation)
		if parseErr != nil || !validOperationID(updateID) || update.objectType != string(Note) || update.attempted.Valid || len(update.blob) != sha256.Size {
			return ErrDivergentFolderMoveIneligible
		}
		var exactUpdate int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox WHERE operation_id=? AND mutation='update' AND status='pending')`, update.operation).Scan(&exactUpdate); err != nil || exactUpdate != 1 {
			return ErrDivergentFolderMoveIneligible
		}
		if err := requireSingleDependencyTx(ctx, tx, updateID, previous); err != nil {
			return err
		}
		entry := ConflictFolderCreateNoteChainOperation{OperationID: updateID, PreviousOperationID: previous}
		copy(entry.BlobHash[:], update.blob)
		member.Chain = append(member.Chain, entry)
		allowDependency(allowedChildren, previous, updateID)
		previous = updateID
		finalHash = entry.BlobHash
	}
	if finalHash != member.SourceBlobHash {
		return ErrDivergentFolderMoveIneligible
	}
	return nil
}

func requireSingleDependencyTx(ctx context.Context, tx *sql.Tx, operationID, dependencyID uuid.UUID) error {
	var count, exact int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(dependency_operation_id=?),0) FROM sync_outbox_dependencies WHERE operation_id=?`, dependencyID.String(), operationID.String()).Scan(&count, &exact); err != nil {
		return err
	}
	if count != 1 || exact != 1 {
		return ErrDivergentFolderMoveIneligible
	}
	return nil
}

func allowDependency(allowed map[uuid.UUID]map[uuid.UUID]bool, parent, child uuid.UUID) {
	if allowed[parent] == nil {
		allowed[parent] = map[uuid.UUID]bool{}
	}
	allowed[parent][child] = true
}

func validateDivergentTreeDependencyClosureTx(ctx context.Context, tx *sql.Tx, manifest *ConflictFolderDivergentTreeManifest, allowed map[uuid.UUID]map[uuid.UUID]bool) error {
	parents := map[uuid.UUID]bool{manifest.OperationID: true}
	for _, member := range manifest.Members {
		parents[member.SourceOperationID] = true
		for _, chain := range member.Chain {
			parents[chain.OperationID] = true
		}
	}
	for parent := range parents {
		rows, err := tx.QueryContext(ctx, `SELECT child.operation_id FROM sync_outbox_dependencies d JOIN sync_outbox child ON child.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND child.status IN('pending','attempted','replay_mismatch','conflict')`, parent.String())
		if err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return err
			}
			id, err := uuid.Parse(raw)
			if err != nil || !allowed[parent][id] || seen[id] {
				rows.Close()
				return fmt.Errorf("divergent descendant dependency %s -> %s changed: %w", parent, raw, ErrDivergentFolderMoveIneligible)
			}
			seen[id] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(seen) != len(allowed[parent]) {
			return fmt.Errorf("divergent descendant dependency count for %s is %d, want %d: %w", parent, len(seen), len(allowed[parent]), ErrDivergentFolderMoveIneligible)
		}
	}
	return nil
}

func validateDivergentFolderTreeManifestShape(manifest ConflictFolderDivergentTreeManifest, recoveredRootID uuid.UUID) error {
	if !validOperationID(manifest.OperationID) || !validOperationID(manifest.NewRootOperationID) || manifest.OperationID == manifest.NewRootOperationID || !validObjectID(recoveredRootID) || len(manifest.Members) == 0 || len(manifest.Members) > MaxDivergentFolderTreeMembers {
		return errors.New("invalid divergent folder tree manifest")
	}
	seenSource := map[uuid.UUID]bool{}
	seenRecovered := map[uuid.UUID]bool{recoveredRootID: true}
	seenOperations := map[uuid.UUID]bool{manifest.NewRootOperationID: true}
	recoveredBySource := map[uuid.UUID]uuid.UUID{}
	for _, member := range manifest.Members {
		if !validDivergentFolderTreeMember(member) || seenSource[member.SourceObjectID] || seenRecovered[member.RecoveredObjectID] || seenOperations[member.NewOperationID] {
			return errors.New("invalid divergent folder tree member")
		}
		if member.Depth == 1 {
			if member.RecoveredParentID != recoveredRootID {
				return errors.New("invalid divergent folder tree root parent")
			}
		} else if recoveredBySource[member.SourceParentID] != member.RecoveredParentID {
			return errors.New("invalid divergent folder tree parent mapping")
		}
		seenSource[member.SourceObjectID] = true
		seenRecovered[member.RecoveredObjectID] = true
		seenOperations[member.NewOperationID] = true
		recoveredBySource[member.SourceObjectID] = member.RecoveredObjectID
	}
	return nil
}

func (s *Store) PutConflictFolderDivergentMoveRecoveryWithTree(ctx context.Context, recovery ConflictFolderDivergentMoveRecovery, parentManifest ConflictFolderDivergentParentManifest, manifest ConflictFolderDivergentTreeManifest) error {
	if !validConflictFolderDivergentMoveRecovery(recovery) || recovery.State != "prepared" || manifest.OperationID != recovery.OperationID || manifest.NewRootOperationID != recovery.NewOperationID {
		return errors.New("invalid divergent folder tree recovery")
	}
	if err := validateDivergentFolderTreeManifestShape(manifest, recovery.RecoveredFolderID); err != nil {
		return err
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		if err := validateDivergentFolderTreeTopologyTx(ctx, tx, recovery.FolderID, manifest); err != nil {
			return err
		}
		if err := putConflictFolderDivergentRecoveryTx(ctx, tx, recovery, parentManifest); err != nil {
			return err
		}
		known := 0
		for _, member := range manifest.Members {
			if member.SourceRevision > 0 {
				known++
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_tree_manifests(operation_id,new_root_operation_id,member_count,known_count,sealed) VALUES(?,?,?,?,0)`, manifest.OperationID.String(), manifest.NewRootOperationID.String(), len(manifest.Members), known); err != nil {
			return err
		}
		for ordinal, member := range manifest.Members {
			var device, inode, sourceHash, recoveredHash, createHash any
			if member.ObjectType == Folder {
				device, inode = member.Device, member.Inode
			} else {
				sourceHash, recoveredHash = member.SourceBlobHash[:], member.RecoveredBlobHash[:]
				if member.SourceRevision == 0 {
					createHash = member.CreateBlobHash[:]
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_tree_members(operation_id,ordinal,source_object_id,recovered_object_id,source_parent_id,recovered_parent_id,source_operation_id,new_operation_id,object_type,name,relative_path,depth,source_revision,device,inode,source_blob_hash,recovered_blob_hash,create_blob_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, manifest.OperationID.String(), ordinal+1, member.SourceObjectID.String(), member.RecoveredObjectID.String(), member.SourceParentID.String(), member.RecoveredParentID.String(), member.SourceOperationID.String(), member.NewOperationID.String(), member.ObjectType, member.Name, member.RelativePath, member.Depth, member.SourceRevision, device, inode, sourceHash, recoveredHash, createHash); err != nil {
				return err
			}
			for chainOrdinal, chain := range member.Chain {
				if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_tree_note_chains(operation_id,source_object_id,ordinal,old_operation_id,previous_operation_id,blob_hash) VALUES(?,?,?,?,?,?)`, manifest.OperationID.String(), member.SourceObjectID.String(), chainOrdinal+1, chain.OperationID.String(), chain.PreviousOperationID.String(), chain.BlobHash[:]); err != nil {
					return err
				}
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_tree_manifests SET sealed=1 WHERE operation_id=? AND sealed=0`, manifest.OperationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent folder tree manifest seal unavailable")
		}
		return nil
	})
}

func validateDivergentFolderTreeTopologyTx(ctx context.Context, tx *sql.Tx, rootID uuid.UUID, manifest ConflictFolderDivergentTreeManifest) error {
	allowed := map[uuid.UUID]map[uuid.UUID]bool{}
	sourceOperations := map[uuid.UUID]uuid.UUID{rootID: manifest.OperationID}
	for _, member := range manifest.Members {
		var revision uint64
		var operation sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT revision,operation_id FROM sync_baselines WHERE object_id=?`, member.SourceObjectID.String()).Scan(&revision, &operation)
		if member.SourceRevision > 0 {
			if err != nil || revision != member.SourceRevision || !operation.Valid || operation.String != member.SourceOperationID.String() {
				return fmt.Errorf("divergent known descendant baseline changed for %s: %w", member.RelativePath, ErrDivergentFolderMoveIneligible)
			}
			var open int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=? AND status IN('pending','attempted','replay_mismatch','conflict')`, member.SourceObjectID.String()).Scan(&open); err != nil {
				return err
			}
			if open != 0 {
				return fmt.Errorf("divergent known descendant gained %d open operations for %s: %w", open, member.RelativePath, ErrDivergentFolderMoveIneligible)
			}
		} else {
			if err == nil || !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("divergent local descendant gained baseline for %s: %w", member.RelativePath, ErrDivergentFolderMoveIneligible)
			}
			candidate := ConflictFolderDivergentTreeMember{SourceObjectID: member.SourceObjectID, SourceParentID: member.SourceParentID, ObjectType: member.ObjectType, Name: member.Name, SourceBlobHash: member.SourceBlobHash}
			if err := validateLocalDivergentTreeMemberTx(ctx, tx, candidate, member, sourceOperations[member.SourceParentID], allowed); err != nil {
				return err
			}
		}
		sourceOperations[member.SourceObjectID] = member.SourceOperationID
	}
	return validateDivergentTreeDependencyClosureTx(ctx, tx, &manifest, allowed)
}

func validateLocalDivergentTreeMemberTx(ctx context.Context, tx *sql.Tx, candidate, expected ConflictFolderDivergentTreeMember, parentOperation uuid.UUID, allowed map[uuid.UUID]map[uuid.UUID]bool) error {
	var mutation, objectType, parent, name, status string
	var attempted sql.NullInt64
	var blob []byte
	if err := tx.QueryRowContext(ctx, `SELECT mutation,object_type,parent_id,name,status,attempted_at_ms,blob_hash FROM sync_outbox WHERE operation_id=? AND object_id=?`, expected.SourceOperationID.String(), expected.SourceObjectID.String()).Scan(&mutation, &objectType, &parent, &name, &status, &attempted, &blob); err != nil {
		return err
	}
	if mutation != string(Create) || objectType != string(expected.ObjectType) || parent != expected.SourceParentID.String() || name != expected.Name || status != "pending" || attempted.Valid || expected.ObjectType == Folder && blob != nil || expected.ObjectType == Note && !bytes.Equal(blob, expected.CreateBlobHash[:]) {
		return ErrDivergentFolderMoveIneligible
	}
	if err := requireSingleDependencyTx(ctx, tx, expected.SourceOperationID, parentOperation); err != nil {
		return err
	}
	allowDependency(allowed, parentOperation, expected.SourceOperationID)
	previous := expected.SourceOperationID
	for _, chain := range expected.Chain {
		var gotMutation, gotType, gotStatus string
		var gotAttempted sql.NullInt64
		var gotBlob []byte
		if err := tx.QueryRowContext(ctx, `SELECT mutation,object_type,status,attempted_at_ms,blob_hash FROM sync_outbox WHERE operation_id=? AND object_id=?`, chain.OperationID.String(), expected.SourceObjectID.String()).Scan(&gotMutation, &gotType, &gotStatus, &gotAttempted, &gotBlob); err != nil {
			return err
		}
		if gotMutation != string(Update) || gotType != string(Note) || gotStatus != "pending" || gotAttempted.Valid || chain.PreviousOperationID != previous || !bytes.Equal(gotBlob, chain.BlobHash[:]) {
			return ErrDivergentFolderMoveIneligible
		}
		if err := requireSingleDependencyTx(ctx, tx, chain.OperationID, previous); err != nil {
			return err
		}
		allowDependency(allowed, previous, chain.OperationID)
		previous = chain.OperationID
	}
	var history int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=?`, expected.SourceObjectID.String()).Scan(&history); err != nil || history != 1+len(expected.Chain) {
		return ErrDivergentFolderMoveIneligible
	}
	return nil
}

func (s *Store) ConflictFolderDivergentTreeManifest(ctx context.Context, operationID uuid.UUID) (*ConflictFolderDivergentTreeManifest, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid divergent folder tree lookup")
	}
	var result *ConflictFolderDivergentTreeManifest
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var newRoot string
		var count, sealed int
		if err := tx.QueryRowContext(ctx, `SELECT new_root_operation_id,member_count,sealed FROM conflict_folder_divergent_tree_manifests WHERE operation_id=?`, operationID.String()).Scan(&newRoot, &count, &sealed); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		manifest := &ConflictFolderDivergentTreeManifest{OperationID: operationID}
		var err error
		manifest.NewRootOperationID, err = uuid.Parse(newRoot)
		if err != nil || sealed != 1 || count <= 0 || count > MaxDivergentFolderTreeMembers {
			return errors.New("corrupt divergent folder tree manifest")
		}
		rows, err := tx.QueryContext(ctx, `SELECT m.source_object_id,m.recovered_object_id,m.source_parent_id,m.recovered_parent_id,m.source_operation_id,m.new_operation_id,m.object_type,m.name,m.relative_path,m.depth,m.source_revision,m.device,m.inode,m.source_blob_hash,m.recovered_blob_hash,m.create_blob_hash,c.device,c.inode FROM conflict_folder_divergent_tree_members m LEFT JOIN conflict_folder_divergent_tree_canonical_folders c ON c.operation_id=m.operation_id AND c.source_object_id=m.source_object_id WHERE m.operation_id=? ORDER BY m.ordinal`, operationID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var member ConflictFolderDivergentTreeMember
			var source, recovered, sourceParent, recoveredParent, sourceOperation, newOperation string
			var device, inode, canonicalDevice, canonicalInode sql.NullInt64
			var sourceHash, recoveredHash, createHash []byte
			if err := rows.Scan(&source, &recovered, &sourceParent, &recoveredParent, &sourceOperation, &newOperation, &member.ObjectType, &member.Name, &member.RelativePath, &member.Depth, &member.SourceRevision, &device, &inode, &sourceHash, &recoveredHash, &createHash, &canonicalDevice, &canonicalInode); err != nil {
				return err
			}
			for target, raw := range map[*uuid.UUID]string{&member.SourceObjectID: source, &member.RecoveredObjectID: recovered, &member.SourceParentID: sourceParent, &member.RecoveredParentID: recoveredParent, &member.SourceOperationID: sourceOperation, &member.NewOperationID: newOperation} {
				*target, err = uuid.Parse(raw)
				if err != nil {
					return errors.New("corrupt divergent folder tree identity")
				}
			}
			if device.Valid {
				member.Device, member.Inode = uint64(device.Int64), uint64(inode.Int64)
			}
			if canonicalDevice.Valid {
				member.CanonicalDevice, member.CanonicalInode = uint64(canonicalDevice.Int64), uint64(canonicalInode.Int64)
			}
			if len(sourceHash) != 0 {
				if len(sourceHash) != sha256.Size || len(recoveredHash) != sha256.Size {
					return errors.New("corrupt divergent folder tree hashes")
				}
				copy(member.SourceBlobHash[:], sourceHash)
				copy(member.RecoveredBlobHash[:], recoveredHash)
				if len(createHash) != 0 {
					if len(createHash) != sha256.Size {
						return errors.New("corrupt divergent folder tree create hash")
					}
					copy(member.CreateBlobHash[:], createHash)
				}
			}
			chainRows, err := tx.QueryContext(ctx, `SELECT old_operation_id,previous_operation_id,blob_hash FROM conflict_folder_divergent_tree_note_chains WHERE operation_id=? AND source_object_id=? ORDER BY ordinal`, operationID.String(), source)
			if err != nil {
				return err
			}
			for chainRows.Next() {
				var chain ConflictFolderCreateNoteChainOperation
				var old, previous string
				var hash []byte
				if err := chainRows.Scan(&old, &previous, &hash); err != nil {
					chainRows.Close()
					return err
				}
				chain.OperationID, err = uuid.Parse(old)
				if err == nil {
					chain.PreviousOperationID, err = uuid.Parse(previous)
				}
				if err != nil || len(hash) != sha256.Size {
					chainRows.Close()
					return errors.New("corrupt divergent folder tree chain")
				}
				copy(chain.BlobHash[:], hash)
				member.Chain = append(member.Chain, chain)
			}
			if err := chainRows.Close(); err != nil {
				return err
			}
			manifest.Members = append(manifest.Members, member)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(manifest.Members) != count {
			return errors.New("corrupt divergent folder tree member count")
		}
		result = manifest
		return nil
	})
	return result, err
}

func (s *Store) ValidateConflictFolderDivergentTreeManifest(ctx context.Context, rootID uuid.UUID, manifest ConflictFolderDivergentTreeManifest) error {
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error { return validateDivergentFolderTreeTopologyTx(ctx, tx, rootID, manifest) })
}

func (s *Store) MarkConflictFolderDivergentMoveCanonicalPreparedWithTree(ctx context.Context, operationID uuid.UUID, device, inode uint64, folders map[uuid.UUID][2]uint64) error {
	if !validOperationID(operationID) || device == 0 || inode == 0 || device > math.MaxInt64 || inode > math.MaxInt64 {
		return errors.New("invalid divergent tree canonical identity")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var expected int
		if err := tx.QueryRowContext(ctx, `SELECT known_count FROM conflict_folder_divergent_tree_manifests WHERE operation_id=? AND sealed=1`, operationID.String()).Scan(&expected); err != nil {
			return err
		}
		var knownFolders int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflict_folder_divergent_tree_members WHERE operation_id=? AND source_revision>0 AND object_type='folder'`, operationID.String()).Scan(&knownFolders); err != nil {
			return err
		}
		_ = expected
		if len(folders) != knownFolders {
			return errors.New("incomplete divergent tree canonical folder bindings")
		}
		for source, identity := range folders {
			if !validObjectID(source) || identity[0] == 0 || identity[1] == 0 || identity[0] > math.MaxInt64 || identity[1] > math.MaxInt64 {
				return errors.New("invalid divergent tree canonical folder binding")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_tree_canonical_folders(operation_id,source_object_id,device,inode) SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_members WHERE operation_id=? AND source_object_id=? AND source_revision>0 AND object_type='folder')`, operationID.String(), source.String(), identity[0], identity[1], operationID.String(), source.String()); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_move_recoveries SET state='canonical_prepared',canonical_device=?,canonical_inode=? WHERE operation_id=? AND state='evacuated'`, device, inode, operationID.String())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("divergent tree canonical preparation unavailable")
		}
		return nil
	})
}

func divergentFolderTreeReplacementMutationsTx(ctx context.Context, tx *sql.Tx, recovery ConflictFolderDivergentMoveRecovery) (bool, []Mutation, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_divergent_tree_manifests WHERE operation_id=? AND sealed=1)`, recovery.OperationID.String()).Scan(&exists); err != nil || exists == 0 {
		return false, nil, err
	}
	manifest, err := conflictFolderDivergentTreeManifestTx(ctx, tx, recovery.OperationID)
	if err != nil {
		return true, nil, err
	}
	if err := validateDivergentKnownDescendantsTx(ctx, tx, recovery.OperationID); err != nil {
		return true, nil, err
	}
	if err := validateDivergentFolderTreeBindingsTx(ctx, tx, recovery, *manifest); err != nil {
		return true, nil, err
	}
	parent := ConflictRecoveredID
	mutations := []Mutation{{OperationID: recovery.NewOperationID, Kind: Create, ObjectID: recovery.RecoveredFolderID, ObjectType: Folder, ParentID: &parent, Name: path.Base(recovery.RecoveryRelative)}}
	for _, member := range manifest.Members {
		if member.SourceRevision == 0 {
			for _, operation := range append([]uuid.UUID{member.SourceOperationID}, chainOperationIDs(member.Chain)...) {
				res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND status='pending'`, operation.String())
				if err != nil {
					return true, nil, err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return true, nil, errors.New("divergent tree local history changed")
				}
			}
		}
		parentID := member.RecoveredParentID
		dependency := recovery.NewOperationID
		if member.Depth > 1 {
			for _, candidate := range manifest.Members {
				if candidate.RecoveredObjectID == member.RecoveredParentID {
					dependency = candidate.NewOperationID
					break
				}
			}
		}
		mutation := Mutation{OperationID: member.NewOperationID, Kind: Create, ObjectID: member.RecoveredObjectID, ObjectType: member.ObjectType, ParentID: &parentID, Name: member.Name, DependencyOperationID: &dependency}
		if member.ObjectType == Note {
			mutation.BlobHash = append([]byte(nil), member.RecoveredBlobHash[:]...)
		}
		mutations = append(mutations, mutation)
	}
	return true, mutations, nil
}

func chainOperationIDs(chain []ConflictFolderCreateNoteChainOperation) []uuid.UUID {
	result := make([]uuid.UUID, len(chain))
	for i := range chain {
		result[i] = chain[i].OperationID
	}
	return result
}

func conflictFolderDivergentTreeManifestTx(ctx context.Context, tx *sql.Tx, operationID uuid.UUID) (*ConflictFolderDivergentTreeManifest, error) {
	var newRoot string
	if err := tx.QueryRowContext(ctx, `SELECT new_root_operation_id FROM conflict_folder_divergent_tree_manifests WHERE operation_id=? AND sealed=1`, operationID.String()).Scan(&newRoot); err != nil {
		return nil, err
	}
	manifest := &ConflictFolderDivergentTreeManifest{OperationID: operationID}
	var err error
	manifest.NewRootOperationID, err = uuid.Parse(newRoot)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_object_id,recovered_object_id,source_parent_id,recovered_parent_id,source_operation_id,new_operation_id,object_type,name,relative_path,depth,source_revision,device,inode,source_blob_hash,recovered_blob_hash,create_blob_hash FROM conflict_folder_divergent_tree_members WHERE operation_id=? ORDER BY ordinal`, operationID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var member ConflictFolderDivergentTreeMember
		var source, recovered, sourceParent, recoveredParent, sourceOperation, newOperation string
		var device, inode sql.NullInt64
		var sourceHash, recoveredHash, createHash []byte
		if err := rows.Scan(&source, &recovered, &sourceParent, &recoveredParent, &sourceOperation, &newOperation, &member.ObjectType, &member.Name, &member.RelativePath, &member.Depth, &member.SourceRevision, &device, &inode, &sourceHash, &recoveredHash, &createHash); err != nil {
			return nil, err
		}
		for target, raw := range map[*uuid.UUID]string{&member.SourceObjectID: source, &member.RecoveredObjectID: recovered, &member.SourceParentID: sourceParent, &member.RecoveredParentID: recoveredParent, &member.SourceOperationID: sourceOperation, &member.NewOperationID: newOperation} {
			*target, err = uuid.Parse(raw)
			if err != nil {
				return nil, err
			}
		}
		if device.Valid {
			member.Device, member.Inode = uint64(device.Int64), uint64(inode.Int64)
		} else {
			copy(member.SourceBlobHash[:], sourceHash)
			copy(member.RecoveredBlobHash[:], recoveredHash)
			if len(createHash) == sha256.Size {
				copy(member.CreateBlobHash[:], createHash)
			}
		}
		chainRows, err := tx.QueryContext(ctx, `SELECT old_operation_id,previous_operation_id,blob_hash FROM conflict_folder_divergent_tree_note_chains WHERE operation_id=? AND source_object_id=? ORDER BY ordinal`, operationID.String(), source)
		if err != nil {
			return nil, err
		}
		for chainRows.Next() {
			var entry ConflictFolderCreateNoteChainOperation
			var old, previous string
			var hash []byte
			if err := chainRows.Scan(&old, &previous, &hash); err != nil {
				chainRows.Close()
				return nil, err
			}
			entry.OperationID, err = uuid.Parse(old)
			if err == nil {
				entry.PreviousOperationID, err = uuid.Parse(previous)
			}
			if err != nil || len(hash) != sha256.Size {
				chainRows.Close()
				return nil, errors.New("corrupt divergent tree chain")
			}
			copy(entry.BlobHash[:], hash)
			member.Chain = append(member.Chain, entry)
		}
		chainRows.Close()
		manifest.Members = append(manifest.Members, member)
	}
	return manifest, rows.Err()
}

func validateDivergentKnownDescendantsTx(ctx context.Context, tx *sql.Tx, operationID uuid.UUID) error {
	var changed int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM conflict_folder_divergent_tree_members member
		WHERE member.operation_id=? AND member.source_revision>0 AND (
			NOT EXISTS(SELECT 1 FROM sync_baselines baseline WHERE baseline.object_id=member.source_object_id AND baseline.revision=member.source_revision AND baseline.operation_id=member.source_operation_id)
			OR EXISTS(SELECT 1 FROM sync_outbox pending WHERE pending.object_id=member.source_object_id AND pending.status IN('pending','attempted','replay_mismatch','conflict'))
		)
	)`, operationID.String()).Scan(&changed); err != nil {
		return err
	}
	if changed != 0 {
		return ErrDivergentFolderMoveIneligible
	}
	return nil
}

func validateDivergentFolderTreeBindingsTx(ctx context.Context, tx *sql.Tx, recovery ConflictFolderDivergentMoveRecovery, manifest ConflictFolderDivergentTreeManifest) error {
	if err := requireIndexedTreeObjectTx(ctx, tx, recovery.RecoveredFolderID, Folder, ConflictRecoveredID, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, nil); err != nil {
		return err
	}
	if err := requireIndexedTreeObjectTx(ctx, tx, recovery.FolderID, Folder, recovery.CanonicalParentID, recovery.CanonicalRelative, recovery.CanonicalDevice, recovery.CanonicalInode, nil); err != nil {
		return err
	}
	canonicalFolders := map[uuid.UUID][2]uint64{}
	rows, err := tx.QueryContext(ctx, `SELECT source_object_id,device,inode FROM conflict_folder_divergent_tree_canonical_folders WHERE operation_id=?`, recovery.OperationID.String())
	if err != nil {
		return err
	}
	for rows.Next() {
		var raw string
		var device, inode uint64
		if err := rows.Scan(&raw, &device, &inode); err != nil {
			rows.Close()
			return err
		}
		id, err := uuid.Parse(raw)
		if err != nil {
			rows.Close()
			return err
		}
		canonicalFolders[id] = [2]uint64{device, inode}
	}
	rows.Close()
	for _, member := range manifest.Members {
		recoveryRelative := path.Join(recovery.RecoveryRelative, member.RelativePath)
		var recoveryHash []byte
		if member.ObjectType == Note {
			recoveryHash = member.RecoveredBlobHash[:]
		}
		if err := requireIndexedTreeObjectTx(ctx, tx, member.RecoveredObjectID, member.ObjectType, member.RecoveredParentID, recoveryRelative, member.Device, member.Inode, recoveryHash); err != nil {
			return err
		}
		if member.SourceRevision == 0 {
			continue
		}
		canonicalRelative := path.Join(recovery.CanonicalRelative, member.RelativePath)
		var canonicalHash []byte
		var device, inode uint64
		if member.ObjectType == Note {
			canonicalHash = member.SourceBlobHash[:]
		} else {
			identity, ok := canonicalFolders[member.SourceObjectID]
			if !ok {
				return errors.New("missing divergent tree canonical folder identity")
			}
			device, inode = identity[0], identity[1]
		}
		if err := requireIndexedTreeObjectTx(ctx, tx, member.SourceObjectID, member.ObjectType, member.SourceParentID, canonicalRelative, device, inode, canonicalHash); err != nil {
			return err
		}
	}
	return nil
}

func requireIndexedTreeObjectTx(ctx context.Context, tx *sql.Tx, objectID uuid.UUID, objectType ObjectType, parentID uuid.UUID, relative string, device, inode uint64, hash []byte) error {
	var count int
	var parent any
	if parentID != uuid.Nil {
		parent = parentID.String()
	}
	if objectType == Folder {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE object_id=? AND object_type='folder' AND relative_path=? AND identity_state IN('known','pending') AND folder_device=? AND folder_inode=? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id=?)`, objectID.String(), relative, device, inode, parent, parent).Scan(&count); err != nil {
			return err
		}
	} else {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE object_id=? AND object_type='note' AND relative_path=? AND identity_state='known' AND content_hash=? AND ((parent_id IS NULL AND ? IS NULL) OR parent_id=?)`, objectID.String(), relative, hash, parent, parent).Scan(&count); err != nil {
			return err
		}
	}
	if count != 1 {
		return fmt.Errorf("divergent tree indexed binding mismatch for %s (id=%s type=%s parent=%s device=%d inode=%d hash=%x)", relative, objectID, objectType, parentID, device, inode, hash)
	}
	return nil
}
