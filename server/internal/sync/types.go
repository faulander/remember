// Package sync implements the actor-bound internal synchronization core.
package sync

import (
	"errors"

	"github.com/google/uuid"
)

type ObjectType string
type MutationKind string
type ConflictCode string

const (
	ObjectNote   ObjectType = "note"
	ObjectFolder ObjectType = "folder"

	MutationCreate MutationKind = "create"
	MutationUpdate MutationKind = "update"
	MutationMove   MutationKind = "move"
	MutationDelete MutationKind = "delete"

	ConflictObjectExists         ConflictCode = "object_exists"
	ConflictObjectMissing        ConflictCode = "object_missing"
	ConflictObjectDeleted        ConflictCode = "object_deleted"
	ConflictBaseRevisionMismatch ConflictCode = "base_revision_mismatch"
	ConflictParentUnavailable    ConflictCode = "parent_unavailable"
	ConflictPathCollision        ConflictCode = "path_collision"
	ConflictFolderNotEmpty       ConflictCode = "folder_not_empty"
	ConflictFolderCycle          ConflictCode = "folder_cycle"
	ConflictTypeMismatch         ConflictCode = "type_mismatch"
)

var (
	ErrInvalidInput            = errors.New("invalid sync input")
	ErrInactiveActor           = errors.New("inactive sync actor")
	ErrBlobUnavailable         = errors.New("content blob unavailable")
	ErrOperationReplayMismatch = errors.New("operation id replayed with different request")
)

type Mutation struct {
	OperationID  uuid.UUID
	Kind         MutationKind
	ObjectID     uuid.UUID
	ObjectType   ObjectType
	BaseRevision uint64
	ParentID     *uuid.UUID
	Name         string
	BlobHash     []byte
}

type CanonicalState struct {
	ObjectType ObjectType
	Revision   uint64
	ParentID   *uuid.UUID
	Name       string
	BlobHash   []byte
	Deleted    bool
}

type SubmitResult struct {
	Accepted  bool
	Conflict  ConflictCode
	Revision  uint64
	Cursor    uint64
	Canonical *CanonicalState
}

type VersionState struct {
	Cursor      uint64
	Mutation    MutationKind
	OperationID uuid.UUID
	ObjectID    uuid.UUID
	ObjectType  ObjectType
	Revision    uint64
	ParentID    *uuid.UUID
	Name        string
	BlobHash    []byte
	Deleted     bool
}

type PullResult struct {
	Changes    []VersionState
	HasMore    bool
	NextCursor uint64
}
