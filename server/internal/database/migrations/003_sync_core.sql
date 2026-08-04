CREATE TABLE devices (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    id BLOB NOT NULL CHECK(typeof(id) = 'blob' AND length(id) = 16),
    display_name TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('active', 'revoked')),
    created_at_ms INTEGER NOT NULL,
    revoked_at_ms INTEGER,
    PRIMARY KEY(user_id, id),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE content_blobs (
    hash BLOB PRIMARY KEY CHECK(typeof(hash) = 'blob' AND length(hash) = 32),
    size_bytes INTEGER NOT NULL CHECK(size_bytes >= 0 AND size_bytes <= 8388608),
    available INTEGER NOT NULL CHECK(available IN (0, 1)),
    created_at_ms INTEGER NOT NULL
);

CREATE TABLE user_content_blobs (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    hash BLOB NOT NULL CHECK(typeof(hash) = 'blob' AND length(hash) = 32),
    entitled_at_ms INTEGER NOT NULL,
    PRIMARY KEY(user_id, hash),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(hash) REFERENCES content_blobs(hash)
);

CREATE TABLE user_cursor_counters (
    user_id BLOB PRIMARY KEY CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    last_cursor INTEGER NOT NULL CHECK(last_cursor >= 0),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE sync_objects (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    object_id BLOB NOT NULL CHECK(typeof(object_id) = 'blob' AND length(object_id) = 16),
    object_type TEXT NOT NULL CHECK(object_type IN ('note', 'folder')),
    revision INTEGER NOT NULL CHECK(revision > 0),
    parent_id BLOB CHECK(parent_id IS NULL OR (typeof(parent_id) = 'blob' AND length(parent_id) = 16)),
    parent_key BLOB NOT NULL CHECK(typeof(parent_key) = 'blob' AND length(parent_key) = 16),
    name TEXT NOT NULL,
    name_key TEXT NOT NULL COLLATE BINARY,
    blob_hash BLOB CHECK(blob_hash IS NULL OR (typeof(blob_hash) = 'blob' AND length(blob_hash) = 32)),
    deleted INTEGER NOT NULL CHECK(deleted IN (0, 1)),
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL,
    PRIMARY KEY(user_id, object_id),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    FOREIGN KEY(user_id, parent_id) REFERENCES sync_objects(user_id, object_id),
    FOREIGN KEY(user_id, blob_hash) REFERENCES user_content_blobs(user_id, hash),
    CHECK((parent_id IS NULL AND parent_key = zeroblob(16)) OR
          (parent_id IS NOT NULL AND parent_key = parent_id)),
    CHECK((object_type = 'folder' AND blob_hash IS NULL) OR
          (object_type = 'note' AND (deleted = 1 OR blob_hash IS NOT NULL)))
);

CREATE UNIQUE INDEX sync_objects_active_path_uq
    ON sync_objects(user_id, parent_key, name_key) WHERE deleted = 0;
CREATE INDEX sync_objects_parent_idx ON sync_objects(user_id, parent_id) WHERE deleted = 0;

CREATE TABLE sync_operations (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    device_id BLOB NOT NULL CHECK(typeof(device_id) = 'blob' AND length(device_id) = 16),
    operation_id BLOB NOT NULL CHECK(typeof(operation_id) = 'blob' AND length(operation_id) = 16),
    request_hash BLOB NOT NULL CHECK(typeof(request_hash) = 'blob' AND length(request_hash) = 32),
    mutation TEXT NOT NULL CHECK(mutation IN ('create', 'update', 'move', 'delete')),
    object_id BLOB NOT NULL CHECK(typeof(object_id) = 'blob' AND length(object_id) = 16),
    proposed_type TEXT NOT NULL CHECK(proposed_type IN ('note', 'folder')),
    proposed_base_revision INTEGER NOT NULL CHECK(proposed_base_revision >= 0),
    proposed_parent_id BLOB CHECK(proposed_parent_id IS NULL OR (typeof(proposed_parent_id) = 'blob' AND length(proposed_parent_id) = 16)),
    proposed_name TEXT,
    proposed_name_key TEXT COLLATE BINARY,
    proposed_blob_hash BLOB CHECK(proposed_blob_hash IS NULL OR (typeof(proposed_blob_hash) = 'blob' AND length(proposed_blob_hash) = 32)),
    result TEXT NOT NULL CHECK(result IN ('accepted', 'conflict')),
    conflict_code TEXT CHECK(conflict_code IS NULL OR conflict_code IN (
        'object_exists', 'object_missing', 'object_deleted', 'base_revision_mismatch',
        'parent_unavailable', 'path_collision', 'folder_not_empty', 'folder_cycle', 'type_mismatch'
    )),
    result_revision INTEGER CHECK(result_revision IS NULL OR result_revision > 0),
    result_cursor INTEGER CHECK(result_cursor IS NULL OR result_cursor > 0),
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY(user_id, operation_id),
    UNIQUE(user_id, operation_id, object_id, result_revision, result_cursor),
    FOREIGN KEY(user_id, device_id) REFERENCES devices(user_id, id),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    CHECK((result = 'accepted' AND conflict_code IS NULL AND result_revision IS NOT NULL AND result_cursor IS NOT NULL) OR
          (result = 'conflict' AND conflict_code IS NOT NULL AND result_revision IS NULL AND result_cursor IS NULL))
);

CREATE TABLE sync_object_versions (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    object_id BLOB NOT NULL CHECK(typeof(object_id) = 'blob' AND length(object_id) = 16),
    revision INTEGER NOT NULL CHECK(revision > 0),
    operation_id BLOB NOT NULL CHECK(typeof(operation_id) = 'blob' AND length(operation_id) = 16),
    object_type TEXT NOT NULL CHECK(object_type IN ('note', 'folder')),
    parent_id BLOB CHECK(parent_id IS NULL OR (typeof(parent_id) = 'blob' AND length(parent_id) = 16)),
    name TEXT NOT NULL,
    name_key TEXT NOT NULL COLLATE BINARY,
    blob_hash BLOB CHECK(blob_hash IS NULL OR (typeof(blob_hash) = 'blob' AND length(blob_hash) = 32)),
    deleted INTEGER NOT NULL CHECK(deleted IN (0, 1)),
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY(user_id, object_id, revision),
    UNIQUE(user_id, operation_id, object_id, revision),
    FOREIGN KEY(user_id, object_id) REFERENCES sync_objects(user_id, object_id) ON DELETE CASCADE,
    FOREIGN KEY(user_id, operation_id) REFERENCES sync_operations(user_id, operation_id),
    FOREIGN KEY(user_id, blob_hash) REFERENCES user_content_blobs(user_id, hash),
    CHECK((object_type = 'folder' AND blob_hash IS NULL) OR
          (object_type = 'note' AND (deleted = 1 OR blob_hash IS NOT NULL)))
);

CREATE TABLE sync_change_log (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    cursor INTEGER NOT NULL CHECK(cursor > 0),
    object_id BLOB NOT NULL CHECK(typeof(object_id) = 'blob' AND length(object_id) = 16),
    revision INTEGER NOT NULL CHECK(revision > 0),
    operation_id BLOB NOT NULL CHECK(typeof(operation_id) = 'blob' AND length(operation_id) = 16),
    mutation TEXT NOT NULL CHECK(mutation IN ('create', 'update', 'move', 'delete')),
    created_at_ms INTEGER NOT NULL,
    PRIMARY KEY(user_id, cursor),
    UNIQUE(user_id, operation_id),
    FOREIGN KEY(user_id, object_id, revision) REFERENCES sync_object_versions(user_id, object_id, revision),
    FOREIGN KEY(user_id, operation_id, object_id, revision)
        REFERENCES sync_object_versions(user_id, operation_id, object_id, revision),
    FOREIGN KEY(user_id, operation_id, object_id, revision, cursor)
        REFERENCES sync_operations(user_id, operation_id, object_id, result_revision, result_cursor),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TRIGGER sync_object_versions_no_update
BEFORE UPDATE ON sync_object_versions BEGIN
    SELECT RAISE(ABORT, 'sync object versions are immutable');
END;
CREATE TRIGGER sync_object_versions_no_delete
BEFORE DELETE ON sync_object_versions
WHEN EXISTS(SELECT 1 FROM users WHERE id = OLD.user_id) BEGIN
    SELECT RAISE(ABORT, 'sync object versions are immutable');
END;
CREATE TRIGGER sync_change_log_no_update
BEFORE UPDATE ON sync_change_log BEGIN
    SELECT RAISE(ABORT, 'sync change log is immutable');
END;
CREATE TRIGGER sync_change_log_no_delete
BEFORE DELETE ON sync_change_log
WHEN EXISTS(SELECT 1 FROM users WHERE id = OLD.user_id) BEGIN
    SELECT RAISE(ABORT, 'sync change log is immutable');
END;
