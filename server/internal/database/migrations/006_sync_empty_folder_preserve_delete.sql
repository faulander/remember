CREATE TABLE sync_folder_preserve_delete_resolutions(
 user_id BLOB NOT NULL CHECK(typeof(user_id)='blob' AND length(user_id)=16),
 device_id BLOB NOT NULL CHECK(typeof(device_id)='blob' AND length(device_id)=16),
 resolution_operation_id BLOB NOT NULL CHECK(typeof(resolution_operation_id)='blob' AND length(resolution_operation_id)=16),
 request_hash BLOB NOT NULL CHECK(typeof(request_hash)='blob' AND length(request_hash)=32),
 conflict_operation_id BLOB NOT NULL CHECK(typeof(conflict_operation_id)='blob' AND length(conflict_operation_id)=16),
 folder_id BLOB NOT NULL CHECK(typeof(folder_id)='blob' AND length(folder_id)=16),
 expected_revision INTEGER NOT NULL CHECK(expected_revision>0),
 recovered_folder_id BLOB NOT NULL CHECK(typeof(recovered_folder_id)='blob' AND length(recovered_folder_id)=16),
 recovered_cursor INTEGER NOT NULL CHECK(recovered_cursor>0),deleted_cursor INTEGER NOT NULL CHECK(deleted_cursor>recovered_cursor),status TEXT NOT NULL CHECK(status='completed'),created_at_ms INTEGER NOT NULL,
 PRIMARY KEY(user_id,resolution_operation_id),
 FOREIGN KEY(user_id,device_id) REFERENCES devices(user_id,id),FOREIGN KEY(user_id,conflict_operation_id) REFERENCES sync_operations(user_id,operation_id),FOREIGN KEY(user_id,recovered_folder_id) REFERENCES sync_objects(user_id,object_id),FOREIGN KEY(user_id,recovered_cursor) REFERENCES sync_change_log(user_id,cursor),FOREIGN KEY(user_id,deleted_cursor) REFERENCES sync_change_log(user_id,cursor),FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE);
CREATE TRIGGER sync_folder_preserve_delete_resolution_no_update BEFORE UPDATE ON sync_folder_preserve_delete_resolutions BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution is immutable'); END;
CREATE TRIGGER sync_folder_preserve_delete_resolution_no_delete BEFORE DELETE ON sync_folder_preserve_delete_resolutions WHEN EXISTS(SELECT 1 FROM users WHERE id=OLD.user_id) BEGIN SELECT RAISE(ABORT,'folder preserve delete resolution is immutable'); END;
