package reconcile

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path"
	"sort"

	"github.com/faulander/remember/client/internal/clientsync"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

func captureSync(ctx context.Context, root string, index *localindex.Index, previous localindex.Snapshot, snapshot *localindex.Snapshot, options Options) error {
	store, err := clientsync.NewStore(index)
	if err != nil {
		return err
	}
	generator := options.NewOperationID
	if generator == nil {
		generator = uuid.NewV7
	}
	return store.CaptureSnapshot(ctx, snapshot, options.RecoveryMode, func(tx *sql.Tx) ([]clientsync.Mutation, []uuid.UUID, error) {
		old := make(map[uuid.UUID]localindex.Object)
		current := make(map[uuid.UUID]localindex.Object)
		pending := make(map[uuid.UUID]bool)
		for _, o := range previous.Objects {
			if o.IdentityState != localindex.IdentityPending {
				old[o.ID] = o
			}
		}
		for _, o := range snapshot.Objects {
			if o.IdentityState == localindex.IdentityPending {
				pending[o.ID] = true
			} else {
				current[o.ID] = o
			}
		}
		type projection struct {
			revision   uint64
			dependency *uuid.UUID
			durable    bool
		}
		projections := make(map[uuid.UUID]projection)
		loadProjection := func(id uuid.UUID) (projection, error) {
			if p, ok := projections[id]; ok {
				return p, nil
			}
			rev, dep, durable, err := store.ProjectionTx(ctx, tx, id)
			p := projection{rev, dep, durable}
			projections[id] = p
			return p, err
		}
		var creates, changes, deletes []localindex.Object
		for id, o := range current {
			if options.AppliedRemoteFolders[id] && o.Type == localindex.ObjectFolder {
				continue
			}
			if trustedID, ok := options.TrustedRemoteFolders[o.RelativePath]; ok && trustedID == id && o.Type == localindex.ObjectFolder {
				continue
			}
			if expected, ok := options.AppliedRemoteNotes[id]; ok && bytes.Equal(o.ContentHash, expected[:]) {
				continue
			}
			before, was := old[id]
			projected, err := loadProjection(id)
			if err != nil {
				return nil, nil, err
			}
			if !was && projected.durable {
				// The object temporarily disappeared from an observed snapshot but
				// already has durable remote intent. Continue from that projection
				// instead of emitting a second Create.
				changes = append(changes, o)
			} else if !was {
				creates = append(creates, o)
			} else if projected.durable {
				changes = append(changes, o)
			} else if !sameObserved(before, o) {
				creates = append(creates, o)
			}
		}
		for _, issue := range previous.Issues {
			if issue.Code != "sync_blob_too_large" {
				continue
			}
			for id, before := range old {
				after, exists := current[id]
				if exists && before.RelativePath == issue.RelativePath && sameObserved(before, after) {
					snapshot.Issues = append(snapshot.Issues, issue)
				}
			}
		}
		var cancel []uuid.UUID
		for id, o := range old {
			if _, ok := current[id]; ok {
				continue
			}
			if pending[id] {
				continue
			}
			if options.AppliedRemoteDeletes[id] {
				continue
			}
			projected, err := loadProjection(id)
			if err != nil {
				return nil, nil, err
			}
			if projected.durable {
				deletes = append(deletes, o)
			} else {
				cancel = append(cancel, id)
			}
		}
		sort.Slice(creates, func(i, j int) bool {
			return depth(creates[i].RelativePath) < depth(creates[j].RelativePath) || (depth(creates[i].RelativePath) == depth(creates[j].RelativePath) && creates[i].RelativePath < creates[j].RelativePath)
		})
		sort.Slice(changes, func(i, j int) bool { return changes[i].RelativePath < changes[j].RelativePath })
		sort.Slice(deletes, func(i, j int) bool {
			return depth(deletes[i].RelativePath) > depth(deletes[j].RelativePath) || (depth(deletes[i].RelativePath) == depth(deletes[j].RelativePath) && deletes[i].RelativePath < deletes[j].RelativePath)
		})
		var mutations []clientsync.Mutation
		createOps := make(map[uuid.UUID]uuid.UUID)
		for _, o := range creates {
			id, err := generator()
			if err != nil {
				return nil, nil, err
			}
			m := clientsync.Mutation{OperationID: id, Kind: clientsync.Create, ObjectID: o.ID, ObjectType: syncType(o.Type), Name: path.Base(o.RelativePath), BlobHash: append([]byte(nil), o.ContentHash...)}
			if o.ParentID != uuid.Nil {
				p := o.ParentID
				m.ParentID = &p
				if dep, ok := createOps[p]; ok {
					m.DependencyOperationID = &dep
				} else {
					parentProjection, err := loadProjection(p)
					if err != nil {
						return nil, nil, err
					}
					if parentProjection.dependency != nil {
						m.DependencyOperationID = parentProjection.dependency
					} else if !parentProjection.durable {
						return nil, nil, errors.New("sync child parent has no durable server projection")
					}
				}
			}
			if o.Type == localindex.ObjectNote {
				var hash [32]byte
				copy(hash[:], o.ContentHash)
				if err := clientsync.StageNote(root, o.RelativePath, hash); err != nil {
					if errors.Is(err, clientsync.ErrBlobTooLarge) {
						snapshot.Issues = append(snapshot.Issues, localindex.Issue{Code: "sync_blob_too_large", RelativePath: o.RelativePath, Detail: "note exceeds the 8 MiB sync limit"})
						continue
					}
					return nil, nil, err
				}
			}
			mutations = append(mutations, m)
			createOps[o.ID] = id
		}
		for _, o := range changes {
			before := old[o.ID]
			projected, err := loadProjection(o.ID)
			if err != nil {
				return nil, nil, err
			}
			base := projected.revision
			moved := before.ParentID != o.ParentID || path.Base(before.RelativePath) != path.Base(o.RelativePath)
			changed := o.Type == localindex.ObjectNote && !bytes.Equal(before.ContentHash, o.ContentHash)
			dependency := projected.dependency
			if moved {
				id, err := generator()
				if err != nil {
					return nil, nil, err
				}
				m := clientsync.Mutation{OperationID: id, Kind: clientsync.Move, ObjectID: o.ID, ObjectType: syncType(o.Type), BaseRevision: base, Name: path.Base(o.RelativePath), DependencyOperationID: dependency}
				if o.ParentID != uuid.Nil {
					p := o.ParentID
					m.ParentID = &p
					parentProjection, err := loadProjection(p)
					if err != nil {
						return nil, nil, err
					}
					if parentProjection.dependency != nil && (m.DependencyOperationID == nil || *m.DependencyOperationID != *parentProjection.dependency) {
						m.AdditionalDependencies = append(m.AdditionalDependencies, *parentProjection.dependency)
					} else if !parentProjection.durable && parentProjection.dependency == nil {
						return nil, nil, errors.New("sync move parent has no durable server projection")
					}
				}
				mutations = append(mutations, m)
				dependency = &id
				base++
			}
			if changed {
				id, err := generator()
				if err != nil {
					return nil, nil, err
				}
				var hash [32]byte
				copy(hash[:], o.ContentHash)
				if err := clientsync.StageNote(root, o.RelativePath, hash); err != nil {
					if errors.Is(err, clientsync.ErrBlobTooLarge) {
						snapshot.Issues = append(snapshot.Issues, localindex.Issue{Code: "sync_blob_too_large", RelativePath: o.RelativePath, Detail: "note exceeds the 8 MiB sync limit"})
						continue
					}
					return nil, nil, err
				}
				mutations = append(mutations, clientsync.Mutation{OperationID: id, Kind: clientsync.Update, ObjectID: o.ID, ObjectType: clientsync.Note, BaseRevision: base, BlobHash: append([]byte(nil), o.ContentHash...), DependencyOperationID: dependency})
			}
		}
		var lastDelete *uuid.UUID
		for _, o := range deletes {
			projected, err := loadProjection(o.ID)
			if err != nil {
				return nil, nil, err
			}
			base := projected.revision
			id, err := generator()
			if err != nil {
				return nil, nil, err
			}
			mutation := clientsync.Mutation{OperationID: id, Kind: clientsync.Delete, ObjectID: o.ID, ObjectType: syncType(o.Type), BaseRevision: base, DependencyOperationID: projected.dependency}
			if lastDelete != nil && (projected.dependency == nil || *lastDelete != *projected.dependency) {
				mutation.AdditionalDependencies = []uuid.UUID{*lastDelete}
			}
			mutations = append(mutations, mutation)
			copyID := id
			lastDelete = &copyID
		}
		return mutations, cancel, nil
	})
}
func sameObserved(a, b localindex.Object) bool {
	return a.Type == b.Type && a.RelativePath == b.RelativePath && a.ParentID == b.ParentID && bytes.Equal(a.ContentHash, b.ContentHash)
}
func syncType(t localindex.ObjectType) clientsync.ObjectType {
	if t == localindex.ObjectFolder {
		return clientsync.Folder
	}
	return clientsync.Note
}
func depth(p string) int {
	n := 1
	for _, r := range p {
		if r == '/' {
			n++
		}
	}
	return n
}
