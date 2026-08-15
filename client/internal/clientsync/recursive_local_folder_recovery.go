package clientsync

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type RecursiveLocalFolderRecoveryKind string

const (
	RecursiveFolderCreateRecovery        RecursiveLocalFolderRecoveryKind = "folder_create"
	RecursiveFolderDivergentMoveRecovery RecursiveLocalFolderRecoveryKind = "divergent_move"
	RecursiveFolderMoveDeleteRecovery    RecursiveLocalFolderRecoveryKind = "move_delete"
)

type ConflictRecursiveLocalFolderMember struct {
	ObjectID, ParentID, OldOperationID, NewOperationID uuid.UUID
	ObjectType                                         ObjectType
	Name, RelativePath                                 string
	Depth                                              int
	Device, Inode                                      uint64
	CreateBlobHash, FinalBlobHash                      [32]byte
	Chain                                              []ConflictFolderCreateNoteChainOperation
}

type ConflictRecursiveLocalFolderManifest struct {
	Kind               RecursiveLocalFolderRecoveryKind
	OperationID        uuid.UUID
	NewRootOperationID uuid.UUID
	Members            []ConflictRecursiveLocalFolderMember
}

func validRecursiveRecoveryKind(kind RecursiveLocalFolderRecoveryKind) bool {
	return kind == RecursiveFolderCreateRecovery || kind == RecursiveFolderDivergentMoveRecovery || kind == RecursiveFolderMoveDeleteRecovery
}

func validRecursiveMember(m ConflictRecursiveLocalFolderMember) bool {
	if !validObjectID(m.ObjectID) || !validObjectID(m.ParentID) || !validOperationID(m.OldOperationID) || !validOperationID(m.NewOperationID) || m.OldOperationID == m.NewOperationID || m.Depth < 1 || naming.ValidateComponent(m.Name) != nil || naming.ValidateRelativePath(m.RelativePath) != nil || path.Base(m.RelativePath) != m.Name {
		return false
	}
	zero := [32]byte{}
	switch m.ObjectType {
	case Folder:
		return m.Device > 0 && m.Inode > 0 && m.Device <= math.MaxInt64 && m.Inode <= math.MaxInt64 && m.CreateBlobHash == zero && m.FinalBlobHash == zero && len(m.Chain) == 0
	case Note:
		return m.Device == 0 && m.Inode == 0
	default:
		return false
	}
}

// DiscoverRecursiveLocalFolderManifest transactionally captures the complete pending
// create/update DAG below rootOperationID. A nil result means there is no descendant
// folder, so the established empty/direct-note recovery path remains applicable.
func (s *Store) DiscoverRecursiveLocalFolderManifest(ctx context.Context, rootOperationID, rootID uuid.UUID) (*ConflictRecursiveLocalFolderManifest, error) {
	if !validOperationID(rootOperationID) || !validObjectID(rootID) {
		return nil, errors.New("invalid recursive local folder recovery root")
	}
	var result *ConflictRecursiveLocalFolderManifest
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var nested int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox_dependencies d JOIN sync_outbox o ON o.operation_id=d.operation_id WHERE d.dependency_operation_id=? AND o.mutation='create' AND o.object_type='folder' AND o.status='pending' AND o.attempted_at_ms IS NULL)`, rootOperationID.String()).Scan(&nested); err != nil {
			return err
		}
		if nested == 0 {
			return nil
		}
		manifest := &ConflictRecursiveLocalFolderManifest{OperationID: rootOperationID}
		newRoot, err := uuid.NewV7()
		if err != nil {
			return err
		}
		manifest.NewRootOperationID = newRoot
		seenObjects := map[uuid.UUID]bool{}
		var walkFolder func(uuid.UUID, uuid.UUID, string, int) error
		walkFolder = func(parentOperationID, parentObjectID uuid.UUID, parentRelative string, depth int) error {
			rows, err := tx.QueryContext(ctx, `SELECT o.operation_id,o.object_id,o.mutation,o.object_type,o.status,o.attempted_at_ms,o.parent_id,o.name,o.blob_hash,(SELECT COUNT(*) FROM sync_outbox_dependencies x WHERE x.operation_id=o.operation_id) FROM sync_outbox_dependencies d JOIN sync_outbox o ON o.operation_id=d.operation_id WHERE d.dependency_operation_id=? ORDER BY o.sequence`, parentOperationID.String())
			if err != nil {
				return err
			}
			type child struct {
				operation, object, mutation, objectType, status string
				attempted                                       sql.NullInt64
				parent                                          sql.NullString
				name                                            string
				blob                                            []byte
				dependencies                                    int
			}
			var children []child
			for rows.Next() {
				var child child
				if err := rows.Scan(&child.operation, &child.object, &child.mutation, &child.objectType, &child.status, &child.attempted, &child.parent, &child.name, &child.blob, &child.dependencies); err != nil {
					rows.Close()
					return err
				}
				children = append(children, child)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			if err := rows.Close(); err != nil {
				return err
			}
			for _, child := range children {
				if child.mutation != string(Create) || (child.objectType != string(Folder) && child.objectType != string(Note)) || child.status != "pending" || child.attempted.Valid || !child.parent.Valid || child.parent.String != parentObjectID.String() || child.dependencies != 1 || naming.ValidateComponent(child.name) != nil {
					return errors.New("recursive local folder recovery has unsupported descendant operation")
				}
				operationID, err := uuid.Parse(child.operation)
				if err != nil {
					return errors.New("corrupt recursive descendant operation id")
				}
				objectID, err := uuid.Parse(child.object)
				if err != nil || !validObjectID(objectID) || seenObjects[objectID] {
					return errors.New("recursive local folder recovery has duplicate object")
				}
				seenObjects[objectID] = true
				newOperationID, err := uuid.NewV7()
				if err != nil {
					return err
				}
				relative := child.name
				if parentRelative != "" {
					relative = path.Join(parentRelative, child.name)
				}
				member := ConflictRecursiveLocalFolderMember{ObjectID: objectID, ParentID: parentObjectID, OldOperationID: operationID, NewOperationID: newOperationID, ObjectType: ObjectType(child.objectType), Name: child.name, RelativePath: relative, Depth: depth}
				if member.ObjectType == Folder {
					if child.blob != nil {
						return errors.New("recursive folder create has blob")
					}
					manifest.Members = append(manifest.Members, member)
					if err := walkFolder(operationID, objectID, relative, depth+1); err != nil {
						return err
					}
					continue
				}
				if len(child.blob) != 32 {
					return errors.New("recursive note create hash is invalid")
				}
				copy(member.CreateBlobHash[:], child.blob)
				copy(member.FinalBlobHash[:], child.blob)
				previous := operationID
				for {
					updateRows, err := tx.QueryContext(ctx, `SELECT o.operation_id,o.object_id,o.mutation,o.object_type,o.status,o.attempted_at_ms,o.blob_hash,(SELECT COUNT(*) FROM sync_outbox_dependencies x WHERE x.operation_id=o.operation_id) FROM sync_outbox_dependencies d JOIN sync_outbox o ON o.operation_id=d.operation_id WHERE d.dependency_operation_id=? ORDER BY o.sequence`, previous.String())
					if err != nil {
						return err
					}
					type updateRow struct {
						operation, object, mutation, objectType, status string
						attempted                                       sql.NullInt64
						blob                                            []byte
						dependencies                                    int
					}
					var updates []updateRow
					for updateRows.Next() {
						var update updateRow
						if err := updateRows.Scan(&update.operation, &update.object, &update.mutation, &update.objectType, &update.status, &update.attempted, &update.blob, &update.dependencies); err != nil {
							updateRows.Close()
							return err
						}
						updates = append(updates, update)
					}
					if err := updateRows.Err(); err != nil {
						updateRows.Close()
						return err
					}
					if err := updateRows.Close(); err != nil {
						return err
					}
					if len(updates) == 0 {
						break
					}
					if len(updates) != 1 {
						return errors.New("recursive note update history branches")
					}
					update := updates[0]
					if update.object != objectID.String() || update.mutation != string(Update) || update.objectType != string(Note) || update.status != "pending" || update.attempted.Valid || update.dependencies != 1 || len(update.blob) != 32 {
						return errors.New("recursive note history is not a pending linear update chain")
					}
					updateID, err := uuid.Parse(update.operation)
					if err != nil {
						return errors.New("corrupt recursive note update id")
					}
					entry := ConflictFolderCreateNoteChainOperation{OperationID: updateID, PreviousOperationID: previous}
					copy(entry.BlobHash[:], update.blob)
					member.Chain = append(member.Chain, entry)
					member.FinalBlobHash = entry.BlobHash
					previous = updateID
				}
				manifest.Members = append(manifest.Members, member)
			}
			return nil
		}
		if err := walkFolder(rootOperationID, rootID, "", 1); err != nil {
			return err
		}
		for _, member := range manifest.Members {
			var history int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=?`, member.ObjectID.String()).Scan(&history); err != nil {
				return err
			}
			if history != 1+len(member.Chain) {
				return errors.New("recursive local folder object has external history")
			}
			var baseline int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_baselines WHERE object_id=?)`, member.ObjectID.String()).Scan(&baseline); err != nil {
				return err
			}
			if baseline != 0 {
				return errors.New("recursive local folder object has baseline")
			}
		}
		sort.Slice(manifest.Members, func(i, j int) bool {
			if manifest.Members[i].Depth != manifest.Members[j].Depth {
				return manifest.Members[i].Depth < manifest.Members[j].Depth
			}
			return manifest.Members[i].RelativePath < manifest.Members[j].RelativePath
		})
		result = manifest
		return nil
	})
	return result, err
}

func putRecursiveLocalFolderManifestTx(ctx context.Context, tx *sql.Tx, manifest ConflictRecursiveLocalFolderManifest) error {
	if !validRecursiveRecoveryKind(manifest.Kind) || !validOperationID(manifest.OperationID) || !validOperationID(manifest.NewRootOperationID) || len(manifest.Members) == 0 {
		return errors.New("invalid recursive local folder manifest")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_recursive_local_folder_recoveries(recovery_kind,operation_id,new_root_operation_id,member_count) VALUES(?,?,?,?)`, manifest.Kind, manifest.OperationID.String(), manifest.NewRootOperationID.String(), len(manifest.Members)); err != nil {
		return err
	}
	seenObjects := map[uuid.UUID]bool{}
	for _, member := range manifest.Members {
		if !validRecursiveMember(member) || seenObjects[member.ObjectID] {
			return errors.New("invalid recursive local folder member")
		}
		seenObjects[member.ObjectID] = true
		var device, inode, createHash, finalHash any
		if member.ObjectType == Folder {
			device, inode = member.Device, member.Inode
		} else {
			createHash, finalHash = member.CreateBlobHash[:], member.FinalBlobHash[:]
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_recursive_local_folder_members(recovery_kind,operation_id,object_id,object_type,parent_id,name,relative_path,depth,old_operation_id,new_operation_id,device,inode,create_blob_hash,final_blob_hash) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, manifest.Kind, manifest.OperationID.String(), member.ObjectID.String(), member.ObjectType, member.ParentID.String(), member.Name, member.RelativePath, member.Depth, member.OldOperationID.String(), member.NewOperationID.String(), device, inode, createHash, finalHash); err != nil {
			return err
		}
		previous := member.OldOperationID
		for ordinal, chain := range member.Chain {
			if member.ObjectType != Note || !validOperationID(chain.OperationID) || chain.PreviousOperationID != previous {
				return errors.New("invalid recursive local folder note chain")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_recursive_local_folder_note_chains(recovery_kind,operation_id,note_id,ordinal,old_operation_id,previous_operation_id,blob_hash) VALUES(?,?,?,?,?,?,?)`, manifest.Kind, manifest.OperationID.String(), member.ObjectID.String(), ordinal+1, chain.OperationID.String(), chain.PreviousOperationID.String(), chain.BlobHash[:]); err != nil {
				return err
			}
			previous = chain.OperationID
		}
	}
	if err := validateRecursiveLocalFolderTopologyTx(ctx, tx, manifest); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE conflict_recursive_local_folder_recoveries SET sealed=1 WHERE recovery_kind=? AND operation_id=? AND sealed=0`, manifest.Kind, manifest.OperationID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("recursive local folder manifest seal unavailable")
	}
	return nil
}

func (s *Store) ConflictRecursiveLocalFolderManifest(ctx context.Context, kind RecursiveLocalFolderRecoveryKind, operationID uuid.UUID) (*ConflictRecursiveLocalFolderManifest, error) {
	if !validRecursiveRecoveryKind(kind) || !validOperationID(operationID) {
		return nil, errors.New("invalid recursive local folder manifest lookup")
	}
	var result *ConflictRecursiveLocalFolderManifest
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		manifest, err := loadRecursiveLocalFolderManifestTx(ctx, tx, kind, operationID)
		if err != nil {
			return err
		}
		result = manifest
		return nil
	})
	return result, err
}

func loadRecursiveLocalFolderManifestTx(ctx context.Context, tx *sql.Tx, kind RecursiveLocalFolderRecoveryKind, operationID uuid.UUID) (*ConflictRecursiveLocalFolderManifest, error) {
	var rawRoot string
	var count, sealed int
	err := tx.QueryRowContext(ctx, `SELECT new_root_operation_id,member_count,sealed FROM conflict_recursive_local_folder_recoveries WHERE recovery_kind=? AND operation_id=?`, kind, operationID.String()).Scan(&rawRoot, &count, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rootID, err := uuid.Parse(rawRoot)
	if err != nil || !validOperationID(rootID) || count <= 0 || sealed != 1 {
		return nil, errors.New("corrupt recursive local folder manifest root")
	}
	manifest := &ConflictRecursiveLocalFolderManifest{Kind: kind, OperationID: operationID, NewRootOperationID: rootID}
	rows, err := tx.QueryContext(ctx, `SELECT object_id,object_type,parent_id,name,relative_path,depth,old_operation_id,new_operation_id,device,inode,create_blob_hash,final_blob_hash FROM conflict_recursive_local_folder_members WHERE recovery_kind=? AND operation_id=? ORDER BY depth,relative_path`, kind, operationID.String())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var object, objectType, parent, oldOperation, newOperation string
		var member ConflictRecursiveLocalFolderMember
		var device, inode sql.NullInt64
		var createHash, finalHash []byte
		if err := rows.Scan(&object, &objectType, &parent, &member.Name, &member.RelativePath, &member.Depth, &oldOperation, &newOperation, &device, &inode, &createHash, &finalHash); err != nil {
			rows.Close()
			return nil, err
		}
		member.ObjectID, err = uuid.Parse(object)
		if err == nil {
			member.ParentID, err = uuid.Parse(parent)
		}
		if err == nil {
			member.OldOperationID, err = uuid.Parse(oldOperation)
		}
		if err == nil {
			member.NewOperationID, err = uuid.Parse(newOperation)
		}
		member.ObjectType = ObjectType(objectType)
		if device.Valid && device.Int64 > 0 {

			member.Device = uint64(device.Int64)
		}
		if inode.Valid && inode.Int64 > 0 {
			member.Inode = uint64(inode.Int64)
		}
		if len(createHash) == 32 {
			copy(member.CreateBlobHash[:], createHash)
		}
		if len(finalHash) == 32 {
			copy(member.FinalBlobHash[:], finalHash)
		}
		if err != nil || !validRecursiveMember(member) || (member.ObjectType == Note && (len(createHash) != 32 || len(finalHash) != 32)) {
			rows.Close()
			return nil, errors.New("corrupt recursive local folder member")
		}
		chainRows, err := tx.QueryContext(ctx, `SELECT ordinal,old_operation_id,previous_operation_id,blob_hash FROM conflict_recursive_local_folder_note_chains WHERE recovery_kind=? AND operation_id=? AND note_id=? ORDER BY ordinal`, kind, operationID.String(), object)
		if err != nil {
			rows.Close()
			return nil, err
		}
		previous := member.OldOperationID
		for ordinal := 1; chainRows.Next(); ordinal++ {
			var got int
			var old, dependency string
			var hash []byte
			if err := chainRows.Scan(&got, &old, &dependency, &hash); err != nil {
				chainRows.Close()
				rows.Close()
				return nil, err
			}
			entry := ConflictFolderCreateNoteChainOperation{}
			entry.OperationID, err = uuid.Parse(old)
			if err == nil {
				entry.PreviousOperationID, err = uuid.Parse(dependency)
			}
			if err != nil || got != ordinal || entry.PreviousOperationID != previous || len(hash) != 32 || member.ObjectType != Note {
				chainRows.Close()
				rows.Close()
				return nil, errors.New("corrupt recursive local folder note chain")
			}
			copy(entry.BlobHash[:], hash)
			member.Chain = append(member.Chain, entry)
			previous = entry.OperationID
		}
		if err := chainRows.Err(); err != nil {
			chainRows.Close()
			rows.Close()
			return nil, err
		}
		if err := chainRows.Close(); err != nil {
			rows.Close()
			return nil, err
		}
		manifest.Members = append(manifest.Members, member)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(manifest.Members) != count {
		return nil, errors.New("corrupt recursive local folder manifest count")
	}
	if err := validateRecursiveManifestShape(*manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}
func (s *Store) ValidateConflictRecursiveLocalFolderManifest(ctx context.Context, kind RecursiveLocalFolderRecoveryKind, operationID uuid.UUID) error {
	if !validRecursiveRecoveryKind(kind) || !validOperationID(operationID) {
		return errors.New("invalid recursive local folder manifest validation")
	}
	return s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		manifest, err := loadRecursiveLocalFolderManifestTx(ctx, tx, kind, operationID)
		if err != nil {
			return err
		}
		if manifest == nil {
			return errors.New("recursive local folder manifest unavailable")
		}
		return validateRecursiveLocalFolderTopologyTx(ctx, tx, *manifest)
	})
}

func validateRecursiveManifestShape(manifest ConflictRecursiveLocalFolderManifest) error {
	byID := make(map[uuid.UUID]ConflictRecursiveLocalFolderMember, len(manifest.Members))
	seenOperations := map[uuid.UUID]bool{manifest.OperationID: true, manifest.NewRootOperationID: true}
	for _, member := range manifest.Members {
		if !validRecursiveMember(member) || byID[member.ObjectID].ObjectID != uuid.Nil || seenOperations[member.OldOperationID] || seenOperations[member.NewOperationID] {
			return errors.New("invalid recursive local folder manifest shape")
		}
		byID[member.ObjectID] = member
		seenOperations[member.OldOperationID], seenOperations[member.NewOperationID] = true, true
		for _, chain := range member.Chain {
			if seenOperations[chain.OperationID] {
				return errors.New("duplicate recursive local folder operation")
			}
			seenOperations[chain.OperationID] = true
		}
	}
	for _, member := range manifest.Members {
		if member.Depth == 1 {
			if path.Dir(member.RelativePath) != "." {
				return errors.New("recursive local folder direct member path mismatch")
			}
			continue
		}
		parent, ok := byID[member.ParentID]
		if !ok || parent.ObjectType != Folder || parent.Depth+1 != member.Depth || path.Dir(member.RelativePath) != parent.RelativePath {
			return errors.New("recursive local folder parent mismatch")
		}
	}
	return nil
}

func validateRecursiveLocalFolderTopologyTx(ctx context.Context, tx *sql.Tx, manifest ConflictRecursiveLocalFolderManifest) error {
	if err := validateRecursiveManifestShape(manifest); err != nil {
		return err
	}
	var rawRootObject string
	var rootErr error
	switch manifest.Kind {
	case RecursiveFolderCreateRecovery:
		rootErr = tx.QueryRowContext(ctx, `SELECT source_folder_id FROM conflict_folder_create_recoveries WHERE operation_id=?`, manifest.OperationID.String()).Scan(&rawRootObject)
	case RecursiveFolderDivergentMoveRecovery:
		rootErr = tx.QueryRowContext(ctx, `SELECT folder_id FROM conflict_folder_divergent_move_recoveries WHERE operation_id=?`, manifest.OperationID.String()).Scan(&rawRootObject)
	case RecursiveFolderMoveDeleteRecovery:
		rootErr = tx.QueryRowContext(ctx, `SELECT folder_id FROM conflict_folder_move_delete_recoveries WHERE operation_id=?`, manifest.OperationID.String()).Scan(&rawRootObject)
	default:
		return errors.New("invalid recursive local folder recovery kind")
	}
	rootObjectID, parseErr := uuid.Parse(rawRootObject)
	if rootErr != nil || parseErr != nil || !validObjectID(rootObjectID) {
		return errors.New("recursive local folder recovery root changed")
	}
	byID := make(map[uuid.UUID]ConflictRecursiveLocalFolderMember, len(manifest.Members))
	manifestedOld := make(map[string]bool)
	for _, member := range manifest.Members {
		byID[member.ObjectID] = member
		manifestedOld[member.OldOperationID.String()] = true
		for _, chain := range member.Chain {
			manifestedOld[chain.OperationID.String()] = true
		}
	}
	for _, member := range manifest.Members {
		if member.Depth == 1 && member.ParentID != rootObjectID {
			return errors.New("recursive local folder direct parent changed")
		}
		dependency := manifest.OperationID
		if member.Depth > 1 {
			dependency = byID[member.ParentID].OldOperationID
		}
		var mutation, object, objectType, status string
		var attempted sql.NullInt64
		var parent sql.NullString
		var name string
		var blob []byte
		if err := tx.QueryRowContext(ctx, `SELECT mutation,object_id,object_type,status,attempted_at_ms,parent_id,name,blob_hash FROM sync_outbox WHERE operation_id=?`, member.OldOperationID.String()).Scan(&mutation, &object, &objectType, &status, &attempted, &parent, &name, &blob); err != nil {
			return err
		}
		if mutation != string(Create) || object != member.ObjectID.String() || objectType != string(member.ObjectType) || status != "pending" || attempted.Valid || !parent.Valid || parent.String != member.ParentID.String() || name != member.Name || member.ObjectType == Folder && blob != nil || member.ObjectType == Note && !bytes.Equal(blob, member.CreateBlobHash[:]) {
			return errors.New("recursive local folder create changed")
		}
		var dependencies int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE operation_id=? AND dependency_operation_id=?`, member.OldOperationID.String(), dependency.String()).Scan(&dependencies); err != nil || dependencies != 1 {
			return errors.New("recursive local folder create dependency changed")
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox_dependencies WHERE operation_id=?`, member.OldOperationID.String()).Scan(&dependencies); err != nil || dependencies != 1 {
			return errors.New("recursive local folder create has external dependency")
		}
		previous := member.OldOperationID
		finalHash := member.CreateBlobHash
		for _, chain := range member.Chain {
			var valid int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_outbox o WHERE o.operation_id=? AND o.object_id=? AND o.object_type='note' AND o.mutation='update' AND o.status='pending' AND o.attempted_at_ms IS NULL AND o.blob_hash=? AND (SELECT COUNT(*) FROM sync_outbox_dependencies d WHERE d.operation_id=o.operation_id)=1 AND EXISTS(SELECT 1 FROM sync_outbox_dependencies d WHERE d.operation_id=o.operation_id AND d.dependency_operation_id=?))`, chain.OperationID.String(), member.ObjectID.String(), chain.BlobHash[:], previous.String()).Scan(&valid); err != nil || valid != 1 {
				return errors.New("recursive local folder note update changed")
			}
			previous, finalHash = chain.OperationID, chain.BlobHash
		}
		if member.ObjectType == Note && finalHash != member.FinalBlobHash {
			return errors.New("recursive local folder final note hash changed")
		}
		var history int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE object_id=?`, member.ObjectID.String()).Scan(&history); err != nil || history != 1+len(member.Chain) {
			return errors.New("recursive local folder object history changed")
		}
		var baseline int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sync_baselines WHERE object_id=?)`, member.ObjectID.String()).Scan(&baseline); err != nil || baseline != 0 {
			return errors.New("recursive local folder object gained baseline")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT d.operation_id FROM sync_outbox_dependencies d WHERE d.dependency_operation_id=? OR d.dependency_operation_id IN (SELECT old_operation_id FROM conflict_recursive_local_folder_members WHERE recovery_kind=? AND operation_id=?) OR d.dependency_operation_id IN (SELECT old_operation_id FROM conflict_recursive_local_folder_note_chains WHERE recovery_kind=? AND operation_id=?)`, manifest.OperationID.String(), manifest.Kind, manifest.OperationID.String(), manifest.Kind, manifest.OperationID.String())
	if err != nil {
		return err
	}
	for rows.Next() {
		var operation string
		if err := rows.Scan(&operation); err != nil {
			rows.Close()
			return err
		}
		if !manifestedOld[operation] {
			rows.Close()
			return errors.New("recursive local folder recovery has unmanifested dependent")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

func recursiveLocalFolderReplacementMutationsTx(ctx context.Context, tx *sql.Tx, kind RecursiveLocalFolderRecoveryKind, operationID, recoveredRootID uuid.UUID, rootName string) (bool, []Mutation, error) {
	manifest, err := loadRecursiveLocalFolderManifestTx(ctx, tx, kind, operationID)
	if err != nil || manifest == nil {
		return false, nil, err
	}
	if err := validateRecursiveLocalFolderTopologyTx(ctx, tx, *manifest); err != nil {
		return false, nil, err
	}
	for i := len(manifest.Members) - 1; i >= 0; i-- {
		member := manifest.Members[i]
		for j := len(member.Chain) - 1; j >= 0; j-- {
			chain := member.Chain[j]
			res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND object_type='note' AND mutation='update' AND status='pending' AND attempted_at_ms IS NULL AND blob_hash=?`, chain.OperationID.String(), member.ObjectID.String(), chain.BlobHash[:])
			if err != nil {
				return false, nil, err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return false, nil, errors.New("recursive local folder note update retirement changed")
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET status='superseded' WHERE operation_id=? AND object_id=? AND object_type=? AND mutation='create' AND status='pending' AND attempted_at_ms IS NULL`, member.OldOperationID.String(), member.ObjectID.String(), member.ObjectType)
		if err != nil {
			return false, nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, nil, errors.New("recursive local folder create retirement changed")
		}
	}
	parent := ConflictRecoveredID
	mutations := []Mutation{{OperationID: manifest.NewRootOperationID, Kind: Create, ObjectID: recoveredRootID, ObjectType: Folder, ParentID: &parent, Name: rootName}}
	newOperations := make(map[uuid.UUID]uuid.UUID, len(manifest.Members))
	for _, member := range manifest.Members {
		newParentID, dependency := recoveredRootID, manifest.NewRootOperationID
		if member.Depth > 1 {
			newParentID = member.ParentID
			dependency = newOperations[member.ParentID]
			if dependency == uuid.Nil {
				return false, nil, fmt.Errorf("recursive local folder replacement parent missing for %s", member.RelativePath)
			}
		}
		mutation := Mutation{OperationID: member.NewOperationID, Kind: Create, ObjectID: member.ObjectID, ObjectType: member.ObjectType, ParentID: &newParentID, Name: member.Name, DependencyOperationID: &dependency}
		if member.ObjectType == Note {
			mutation.BlobHash = member.FinalBlobHash[:]
		}
		mutations = append(mutations, mutation)
		newOperations[member.ObjectID] = member.NewOperationID
	}
	return true, mutations, nil
}
