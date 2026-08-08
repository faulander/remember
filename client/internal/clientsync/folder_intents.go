package clientsync

import (
	"context"
	"database/sql"
	"errors"

	"github.com/faulander/remember/client/internal/naming"
	"github.com/google/uuid"
)

type OutboxFolderIntent struct {
	OperationID, FolderID uuid.UUID
	Mutation              MutationKind
	SourceRelative        string
	Device, Inode         uint64
}

func (s *Store) AcceptedFolderIntent(ctx context.Context, change Change) (*OutboxFolderIntent, error) {
	if !validOperationID(change.OperationID) || !validObjectID(change.ObjectID) || (change.Mutation != Move && change.Mutation != Delete) || change.ObjectType != Folder || change.Revision == 0 || change.Cursor == 0 {
		return nil, errors.New("invalid accepted folder intent lookup")
	}
	var result *OutboxFolderIntent
	err := s.index.WithTransaction(ctx, func(tx *sql.Tx) error {
		var item OutboxFolderIntent
		var operation, folder string
		err := tx.QueryRowContext(ctx, `SELECT i.operation_id,i.folder_id,i.mutation_kind,i.source_relative,i.device,i.inode FROM sync_outbox_folder_intents i JOIN sync_outbox o ON o.operation_id=i.operation_id WHERE i.operation_id=? AND i.folder_id=? AND i.mutation_kind=? AND o.object_type='folder' AND o.status='accepted' AND o.result_revision=? AND o.result_cursor=?`, change.OperationID.String(), change.ObjectID.String(), change.Mutation, change.Revision, change.Cursor).Scan(&operation, &folder, &item.Mutation, &item.SourceRelative, &item.Device, &item.Inode)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		item.OperationID, err = uuid.Parse(operation)
		if err != nil {
			return err
		}
		item.FolderID, err = uuid.Parse(folder)
		if err != nil || item.OperationID != change.OperationID || item.FolderID != change.ObjectID || item.Mutation != change.Mutation || !validOperationID(item.OperationID) || !validObjectID(item.FolderID) || naming.ValidateUserRelativePath(item.SourceRelative) != nil || item.Device == 0 || item.Inode == 0 {
			return errors.New("corrupt accepted folder intent")
		}
		result = &item
		return nil
	})
	return result, err
}
