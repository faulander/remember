package clientsync

import (
	"context"
	"path"
	"sort"

	"github.com/faulander/remember/client/internal/localindex"
	"github.com/google/uuid"
)

// PrepareBootstrap explicitly adopts an upgraded pre-sync tree. It is never
// called automatically, especially not after local index loss.
func PrepareBootstrap(ctx context.Context, root string, index *localindex.Index, newID func() (uuid.UUID, error)) error {
	if newID == nil {
		newID = uuid.NewV7
	}
	snapshot, err := index.ReadSnapshot(ctx)
	if err != nil {
		return err
	}
	objects := append([]localindex.Object(nil), snapshot.Objects...)
	sort.Slice(objects, func(i, j int) bool {
		return bootstrapDepth(objects[i].RelativePath) < bootstrapDepth(objects[j].RelativePath) || (bootstrapDepth(objects[i].RelativePath) == bootstrapDepth(objects[j].RelativePath) && objects[i].RelativePath < objects[j].RelativePath)
	})
	ops := make([]Mutation, 0, len(objects))
	ids := make(map[uuid.UUID]uuid.UUID)
	for _, o := range objects {
		if o.IdentityState == localindex.IdentityPending {
			continue
		}
		id, err := newID()
		if err != nil {
			return err
		}
		m := Mutation{OperationID: id, Kind: Create, ObjectID: o.ID, ObjectType: Note, Name: path.Base(o.RelativePath), BlobHash: append([]byte(nil), o.ContentHash...)}
		if o.Type == localindex.ObjectFolder {
			m.ObjectType = Folder
			m.BlobHash = nil
		}
		if o.ParentID != uuid.Nil {
			p := o.ParentID
			m.ParentID = &p
			if dep, ok := ids[p]; ok {
				m.DependencyOperationID = &dep
			}
		}
		if o.Type == localindex.ObjectNote {
			var hash [32]byte
			copy(hash[:], o.ContentHash)
			if err := StageNote(root, o.RelativePath, hash); err != nil {
				return err
			}
		}
		ops = append(ops, m)
		ids[o.ID] = id
	}
	store, err := NewStore(index)
	if err != nil {
		return err
	}
	return store.PrepareBootstrap(ctx, ops)
}
func bootstrapDepth(value string) int {
	result := 1
	for _, r := range value {
		if r == '/' {
			result++
		}
	}
	return result
}
