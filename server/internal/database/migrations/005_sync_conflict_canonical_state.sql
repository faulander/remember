ALTER TABLE sync_operations ADD COLUMN conflict_object_type TEXT CHECK(conflict_object_type IS NULL OR conflict_object_type IN ('note','folder'));
ALTER TABLE sync_operations ADD COLUMN conflict_revision INTEGER CHECK(conflict_revision IS NULL OR conflict_revision > 0);
ALTER TABLE sync_operations ADD COLUMN conflict_parent_id BLOB CHECK(conflict_parent_id IS NULL OR length(conflict_parent_id)=16);
ALTER TABLE sync_operations ADD COLUMN conflict_name TEXT;
ALTER TABLE sync_operations ADD COLUMN conflict_blob_hash BLOB CHECK(conflict_blob_hash IS NULL OR length(conflict_blob_hash)=32);
ALTER TABLE sync_operations ADD COLUMN conflict_deleted INTEGER CHECK(conflict_deleted IS NULL OR conflict_deleted IN (0,1));

CREATE TRIGGER sync_operations_conflict_state_shape_insert
BEFORE INSERT ON sync_operations
WHEN (NEW.result='accepted' AND (NEW.conflict_object_type IS NOT NULL OR NEW.conflict_revision IS NOT NULL OR NEW.conflict_parent_id IS NOT NULL OR NEW.conflict_name IS NOT NULL OR NEW.conflict_blob_hash IS NOT NULL OR NEW.conflict_deleted IS NOT NULL))
 OR (NEW.conflict_revision IS NULL AND (NEW.conflict_object_type IS NOT NULL OR NEW.conflict_parent_id IS NOT NULL OR NEW.conflict_name IS NOT NULL OR NEW.conflict_blob_hash IS NOT NULL OR NEW.conflict_deleted IS NOT NULL))
 OR (NEW.conflict_revision IS NOT NULL AND (NEW.result<>'conflict' OR NEW.conflict_object_type IS NULL OR NEW.conflict_name IS NULL OR NEW.conflict_deleted IS NULL))
 OR (NEW.conflict_object_type='note' AND NEW.conflict_blob_hash IS NULL)
 OR (NEW.conflict_object_type='folder' AND NEW.conflict_blob_hash IS NOT NULL)
BEGIN SELECT RAISE(ABORT, 'invalid canonical conflict state'); END;

CREATE TRIGGER sync_operations_immutable_update
BEFORE UPDATE ON sync_operations
BEGIN SELECT RAISE(ABORT, 'sync operations are immutable'); END;
