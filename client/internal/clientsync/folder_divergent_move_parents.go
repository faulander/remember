package clientsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

const MaxDivergentFolderParentDepth = 256

const (
	divergentParentAttempted = "attempted"
	divergentParentCanonical = "canonical"
)

type DivergentFolderParentCandidate struct {
	ObjectID, ParentID uuid.UUID
	RelativePath       string
	Device, Inode      uint64
}

type ConflictFolderDivergentParentBinding struct {
	ObjectID, ParentID  uuid.UUID
	RelativePath        string
	Depth               int
	Revision            uint64
	BaselineOperationID uuid.UUID
	Device, Inode       uint64
}

type ConflictFolderDivergentParentManifest struct {
	OperationID                          uuid.UUID
	AttemptedParentID, CanonicalParentID uuid.UUID
	Attempted, Canonical                 []ConflictFolderDivergentParentBinding
}

func validDivergentParentCandidate(candidate DivergentFolderParentCandidate) bool {
	return validObjectID(candidate.ObjectID) && candidate.ObjectID != candidate.ParentID && naming.ValidateUserRelativePath(candidate.RelativePath) == nil && candidate.Device > 0 && candidate.Inode > 0 && candidate.Device <= math.MaxInt64 && candidate.Inode <= math.MaxInt64
}

func validDivergentParentBinding(binding ConflictFolderDivergentParentBinding) bool {
	return validDivergentParentCandidate(DivergentFolderParentCandidate{ObjectID: binding.ObjectID, ParentID: binding.ParentID, RelativePath: binding.RelativePath, Device: binding.Device, Inode: binding.Inode}) && binding.Depth > 0 && binding.Depth <= MaxDivergentFolderParentDepth && binding.Revision > 0 && binding.Revision <= math.MaxInt64 && validOperationID(binding.BaselineOperationID)
}

func validateDivergentParentChainShape(parentID, rootID uuid.UUID, bindings []ConflictFolderDivergentParentBinding) error {
	if parentID == uuid.Nil {
		if len(bindings) != 0 {
			return errors.New("root divergent parent has bindings")
		}
		return nil
	}
	if !validObjectID(parentID) || len(bindings) == 0 || len(bindings) > MaxDivergentFolderParentDepth || bindings[len(bindings)-1].ObjectID != parentID {
		return errors.New("invalid divergent parent chain")
	}
	seen := map[uuid.UUID]bool{}
	for i, binding := range bindings {
		if !validDivergentParentBinding(binding) || binding.Depth != i+1 || binding.ObjectID == rootID || seen[binding.ObjectID] {
			return errors.New("invalid divergent parent binding")
		}
		if i == 0 {
			if binding.ParentID != uuid.Nil || path.Dir(binding.RelativePath) != "." {
				return errors.New("invalid divergent parent root binding")
			}
		} else if binding.ParentID != bindings[i-1].ObjectID || path.Dir(binding.RelativePath) != bindings[i-1].RelativePath {
			return errors.New("disconnected divergent parent chain")
		}
		seen[binding.ObjectID] = true
	}
	return nil
}

func validateDivergentParentManifestShape(manifest ConflictFolderDivergentParentManifest, rootID uuid.UUID) error {
	if !validOperationID(manifest.OperationID) {
		return errors.New("invalid divergent parent manifest")
	}
	if err := validateDivergentParentChainShape(manifest.AttemptedParentID, rootID, manifest.Attempted); err != nil {
		return err
	}
	return validateDivergentParentChainShape(manifest.CanonicalParentID, rootID, manifest.Canonical)
}

func (s *Store) PlanConflictFolderDivergentParents(ctx context.Context, operationID, rootID, attemptedParentID, canonicalParentID uuid.UUID, attempted, canonical []DivergentFolderParentCandidate) (*ConflictFolderDivergentParentManifest, error) {
	if !validOperationID(operationID) || !validObjectID(rootID) {
		return nil, errors.New("invalid divergent parent plan")
	}
	manifest := &ConflictFolderDivergentParentManifest{OperationID: operationID, AttemptedParentID: attemptedParentID, CanonicalParentID: canonicalParentID}
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var rootValid int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox WHERE operation_id=? AND object_id=? AND mutation='move' AND object_type='folder' AND status='conflict' AND conflict_code='base_revision_mismatch')`, operationID.String(), rootID.String()).Scan(&rootValid); err != nil {
			return err
		}
		if rootValid != 1 {
			return ErrDivergentFolderMoveIneligible
		}
		var err error
		manifest.Attempted, err = planDivergentParentChainTx(ctx, tx, rootID, attemptedParentID, attempted)
		if err != nil {
			return err
		}
		manifest.Canonical, err = planDivergentParentChainTx(ctx, tx, rootID, canonicalParentID, canonical)
		return err
	})
	if err != nil {
		return nil, err
	}
	return manifest, nil
}

func planDivergentParentChainTx(ctx context.Context, tx *sql.Tx, rootID, parentID uuid.UUID, candidates []DivergentFolderParentCandidate) ([]ConflictFolderDivergentParentBinding, error) {
	if parentID == uuid.Nil {
		if len(candidates) != 0 {
			return nil, ErrDivergentFolderMoveIneligible
		}
		return nil, nil
	}
	if !validObjectID(parentID) || len(candidates) == 0 || len(candidates) > MaxDivergentFolderParentDepth || candidates[len(candidates)-1].ObjectID != parentID {
		return nil, ErrDivergentFolderMoveIneligible
	}
	bindings := make([]ConflictFolderDivergentParentBinding, 0, len(candidates))
	seen := map[uuid.UUID]bool{}
	for i, candidate := range candidates {
		if !validDivergentParentCandidate(candidate) || candidate.ObjectID == rootID || seen[candidate.ObjectID] {
			return nil, ErrDivergentFolderMoveIneligible
		}
		if i == 0 {
			if candidate.ParentID != uuid.Nil || path.Dir(candidate.RelativePath) != "." {
				return nil, ErrDivergentFolderMoveIneligible
			}
		} else if candidate.ParentID != candidates[i-1].ObjectID || path.Dir(candidate.RelativePath) != candidates[i-1].RelativePath {
			return nil, ErrDivergentFolderMoveIneligible
		}
		var indexed int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM objects WHERE object_id=? AND object_type='folder' AND parent_id IS ? AND relative_path=? AND identity_state='known' AND folder_device=? AND folder_inode=?`, candidate.ObjectID.String(), nullableUUID(candidate.ParentID), candidate.RelativePath, candidate.Device, candidate.Inode).Scan(&indexed); err != nil {
			return nil, err
		}
		if indexed != 1 {
			return nil, ErrDivergentFolderMoveIneligible
		}
		binding := ConflictFolderDivergentParentBinding{ObjectID: candidate.ObjectID, ParentID: candidate.ParentID, RelativePath: candidate.RelativePath, Depth: i + 1, Device: candidate.Device, Inode: candidate.Inode}
		var baselineOperation string
		if err := tx.QueryRowContext(ctx, `SELECT revision,operation_id FROM sync_baselines WHERE object_id=?`, candidate.ObjectID.String()).Scan(&binding.Revision, &baselineOperation); err != nil {
			return nil, ErrDivergentFolderMoveIneligible
		}
		var err error
		binding.BaselineOperationID, err = uuid.Parse(baselineOperation)
		if err != nil || !validOperationID(binding.BaselineOperationID) {
			return nil, ErrDivergentFolderMoveIneligible
		}
		var open int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=? AND status IN('pending','attempted','replay_mismatch','conflict')`, candidate.ObjectID.String()).Scan(&open); err != nil {
			return nil, err
		}
		if open != 0 {
			return nil, ErrDivergentFolderMoveIneligible
		}
		bindings = append(bindings, binding)
		seen[candidate.ObjectID] = true
	}
	return bindings, nil
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func putConflictFolderDivergentRecoveryTx(ctx context.Context, tx *sql.Tx, recovery ConflictFolderDivergentMoveRecovery, manifest ConflictFolderDivergentParentManifest) error {
	if manifest.OperationID != recovery.OperationID || manifest.AttemptedParentID != recovery.AttemptedParentID || manifest.CanonicalParentID != recovery.CanonicalParentID {
		return errors.New("divergent recovery parent mismatch")
	}
	if err := validateDivergentParentManifestShape(manifest, recovery.FolderID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_move_recoveries(operation_id,folder_id,recovered_folder_id,new_operation_id,attempted_relative,canonical_relative,recovery_relative,source_device,source_inode,canonical_revision,canonical_nonce,attempted_parent_id,canonical_parent_id,state) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,'prepared')`, recovery.OperationID.String(), recovery.FolderID.String(), recovery.RecoveredFolderID.String(), recovery.NewOperationID.String(), recovery.AttemptedRelative, recovery.CanonicalRelative, recovery.RecoveryRelative, recovery.SourceDevice, recovery.SourceInode, recovery.CanonicalRevision, recovery.CanonicalNonce[:], nullableUUID(recovery.AttemptedParentID), nullableUUID(recovery.CanonicalParentID))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("divergent folder move recovery unavailable")
	}
	return putDivergentParentManifestTx(ctx, tx, recovery.FolderID, manifest)
}

func putDivergentParentManifestTx(ctx context.Context, tx *sql.Tx, rootID uuid.UUID, manifest ConflictFolderDivergentParentManifest) error {
	if err := validateDivergentParentManifestShape(manifest, rootID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_parent_manifests(operation_id,attempted_count,canonical_count,sealed) VALUES(?,?,?,0)`, manifest.OperationID.String(), len(manifest.Attempted), len(manifest.Canonical)); err != nil {
		return err
	}
	for _, side := range []struct {
		name     string
		bindings []ConflictFolderDivergentParentBinding
	}{{divergentParentAttempted, manifest.Attempted}, {divergentParentCanonical, manifest.Canonical}} {
		for _, binding := range side.bindings {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_folder_divergent_parent_bindings(operation_id,side,depth,object_id,parent_id,relative_path,revision,baseline_operation_id,device,inode) VALUES(?,?,?,?,?,?,?,?,?,?)`, manifest.OperationID.String(), side.name, binding.Depth, binding.ObjectID.String(), nullableUUID(binding.ParentID), binding.RelativePath, binding.Revision, binding.BaselineOperationID.String(), binding.Device, binding.Inode); err != nil {
				return err
			}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE conflict_folder_divergent_parent_manifests SET sealed=1 WHERE operation_id=? AND sealed=0`, manifest.OperationID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("divergent parent manifest seal unavailable")
	}
	return nil
}

func (s *Store) ConflictFolderDivergentParentManifest(ctx context.Context, operationID uuid.UUID) (*ConflictFolderDivergentParentManifest, error) {
	if !validOperationID(operationID) {
		return nil, errors.New("invalid divergent parent manifest lookup")
	}
	var result *ConflictFolderDivergentParentManifest
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		manifest, err := conflictFolderDivergentParentManifestTx(ctx, tx, operationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result = manifest
		return nil
	})
	return result, err
}

func conflictFolderDivergentParentManifestTx(ctx context.Context, tx *sql.Tx, operationID uuid.UUID) (*ConflictFolderDivergentParentManifest, error) {
	var attemptedParent, canonicalParent sql.NullString
	var attemptedCount, canonicalCount, sealed int
	if err := tx.QueryRowContext(ctx, `SELECT recovery.attempted_parent_id,recovery.canonical_parent_id,manifest.attempted_count,manifest.canonical_count,manifest.sealed FROM conflict_folder_divergent_move_recoveries recovery JOIN conflict_folder_divergent_parent_manifests manifest ON manifest.operation_id=recovery.operation_id WHERE recovery.operation_id=?`, operationID.String()).Scan(&attemptedParent, &canonicalParent, &attemptedCount, &canonicalCount, &sealed); err != nil {
		return nil, err
	}
	manifest := &ConflictFolderDivergentParentManifest{OperationID: operationID}
	var err error
	if attemptedParent.Valid {
		manifest.AttemptedParentID, err = uuid.Parse(attemptedParent.String)
	}
	if err == nil && canonicalParent.Valid {
		manifest.CanonicalParentID, err = uuid.Parse(canonicalParent.String)
	}
	if err != nil || sealed != 1 || attemptedCount < 0 || attemptedCount > MaxDivergentFolderParentDepth || canonicalCount < 0 || canonicalCount > MaxDivergentFolderParentDepth {
		return nil, errors.New("corrupt divergent parent manifest")
	}
	rows, err := tx.QueryContext(ctx, `SELECT side,depth,object_id,parent_id,relative_path,revision,baseline_operation_id,device,inode FROM conflict_folder_divergent_parent_bindings WHERE operation_id=? ORDER BY side,depth`, operationID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var side, objectID, baselineOperation string
		var parentID sql.NullString
		var binding ConflictFolderDivergentParentBinding
		if err := rows.Scan(&side, &binding.Depth, &objectID, &parentID, &binding.RelativePath, &binding.Revision, &baselineOperation, &binding.Device, &binding.Inode); err != nil {
			return nil, err
		}
		binding.ObjectID, err = uuid.Parse(objectID)
		if err == nil && parentID.Valid {
			binding.ParentID, err = uuid.Parse(parentID.String)
		}
		if err == nil {
			binding.BaselineOperationID, err = uuid.Parse(baselineOperation)
		}
		if err != nil || !validDivergentParentBinding(binding) {
			return nil, errors.New("corrupt divergent parent binding")
		}
		switch side {
		case divergentParentAttempted:
			manifest.Attempted = append(manifest.Attempted, binding)
		case divergentParentCanonical:
			manifest.Canonical = append(manifest.Canonical, binding)
		default:
			return nil, errors.New("corrupt divergent parent side")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(manifest.Attempted) != attemptedCount || len(manifest.Canonical) != canonicalCount {
		return nil, errors.New("corrupt divergent parent binding count")
	}
	return manifest, nil
}

func (s *Store) ValidateConflictFolderDivergentParents(ctx context.Context, operationID uuid.UUID) error {
	if !validOperationID(operationID) {
		return errors.New("invalid divergent parent validation")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error { return validateDivergentParentBindingsTx(ctx, tx, operationID) })
}

func validateDivergentParentBindingsTx(ctx context.Context, tx *sql.Tx, operationID uuid.UUID) error {
	var exists, valid int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conflict_folder_divergent_parent_manifests WHERE operation_id=?),EXISTS(SELECT 1 FROM conflict_folder_divergent_parent_manifests_valid WHERE operation_id=?)`, operationID.String(), operationID.String()).Scan(&exists, &valid); err != nil {
		return err
	}
	if exists == 1 {
		if valid != 1 {
			return ErrDivergentFolderMoveIneligible
		}
		return nil
	}
	var attemptedParent, canonicalParent sql.NullString
	var attempted, canonical string
	if err := tx.QueryRowContext(ctx, `SELECT attempted_parent_id,canonical_parent_id,attempted_relative,canonical_relative FROM conflict_folder_divergent_move_recoveries WHERE operation_id=?`, operationID.String()).Scan(&attemptedParent, &canonicalParent, &attempted, &canonical); err != nil {
		return err
	}
	if attemptedParent.Valid || canonicalParent.Valid || path.Dir(attempted) != "." || path.Dir(canonical) != "." {
		return fmt.Errorf("missing divergent parent manifest: %w", ErrDivergentFolderMoveIneligible)
	}
	return nil
}
