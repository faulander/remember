CREATE TABLE sync_baselines (
    object_id TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK(revision >= 0),
    operation_id TEXT,
    CHECK(operation_id IS NULL OR length(operation_id) = 36)
);

CREATE TABLE sync_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE sync_outbox (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    operation_id TEXT NOT NULL UNIQUE,
    mutation TEXT NOT NULL CHECK(mutation IN ('create','update','move','delete')),
    object_id TEXT NOT NULL,
    object_type TEXT NOT NULL CHECK(object_type IN ('note','folder')),
    base_revision INTEGER NOT NULL CHECK(base_revision >= 0),
    parent_id TEXT,
    name TEXT NOT NULL,
    blob_hash BLOB,
    dependency_operation_id TEXT,
    status TEXT NOT NULL CHECK(status IN ('pending','attempted','accepted','conflict','replay_mismatch','superseded')),
    attempted_at_ms INTEGER,
    result_revision INTEGER,
    result_cursor INTEGER,
    conflict_code TEXT,
    created_at_ms INTEGER NOT NULL,
    FOREIGN KEY(dependency_operation_id) REFERENCES sync_outbox(operation_id),
    CHECK(blob_hash IS NULL OR (typeof(blob_hash)='blob' AND length(blob_hash)=32)),
    CHECK((mutation='create' AND base_revision=0 AND name<>'' AND
              ((object_type='note' AND typeof(blob_hash)='blob' AND length(blob_hash)=32) OR
               (object_type='folder' AND blob_hash IS NULL))) OR
          (mutation='update' AND base_revision>0 AND object_type='note' AND parent_id IS NULL AND name='' AND typeof(blob_hash)='blob' AND length(blob_hash)=32) OR
          (mutation='move' AND base_revision>0 AND name<>'' AND blob_hash IS NULL) OR
          (mutation='delete' AND base_revision>0 AND parent_id IS NULL AND name='' AND blob_hash IS NULL)),
    CHECK((status='pending' AND attempted_at_ms IS NULL) OR status<>'pending'),
    CHECK((status='accepted' AND result_revision IS NOT NULL AND result_cursor IS NOT NULL AND conflict_code IS NULL) OR
          (status='conflict' AND result_revision IS NULL AND result_cursor IS NULL AND conflict_code IS NOT NULL) OR
          (status NOT IN ('accepted','conflict') AND result_revision IS NULL AND result_cursor IS NULL AND conflict_code IS NULL))
);
CREATE INDEX sync_outbox_pending_idx ON sync_outbox(status, sequence);
CREATE INDEX sync_outbox_object_idx ON sync_outbox(object_id, sequence);

CREATE TABLE sync_outbox_dependencies (
    operation_id TEXT NOT NULL,
    dependency_operation_id TEXT NOT NULL,
    PRIMARY KEY(operation_id, dependency_operation_id),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id),
    FOREIGN KEY(dependency_operation_id) REFERENCES sync_outbox(operation_id),
    CHECK(operation_id <> dependency_operation_id)
);
CREATE INDEX sync_outbox_dependencies_dependency_idx ON sync_outbox_dependencies(dependency_operation_id);
CREATE TRIGGER sync_outbox_dependencies_no_update
BEFORE UPDATE ON sync_outbox_dependencies BEGIN SELECT RAISE(ABORT, 'sync outbox dependencies are immutable'); END;
CREATE TRIGGER sync_outbox_dependencies_no_delete
BEFORE DELETE ON sync_outbox_dependencies BEGIN SELECT RAISE(ABORT, 'sync outbox dependencies are immutable'); END;

CREATE TRIGGER sync_outbox_payload_immutable
BEFORE UPDATE OF operation_id,mutation,object_id,object_type,base_revision,parent_id,name,blob_hash,dependency_operation_id,created_at_ms
ON sync_outbox BEGIN SELECT RAISE(ABORT, 'sync outbox payload is immutable'); END;
CREATE TRIGGER sync_outbox_no_delete
BEFORE DELETE ON sync_outbox BEGIN SELECT RAISE(ABORT, 'sync outbox history is immutable'); END;

CREATE TABLE apply_plans (
    plan_id TEXT PRIMARY KEY,
    from_cursor INTEGER NOT NULL CHECK(from_cursor >= 0),
    through_cursor INTEGER NOT NULL CHECK(through_cursor >= from_cursor),
    status TEXT NOT NULL CHECK(status IN ('prepared','applying','completed','failed')),
    created_at_ms INTEGER NOT NULL,
    completed_at_ms INTEGER
);
CREATE UNIQUE INDEX apply_plans_one_active ON apply_plans((1)) WHERE status IN ('prepared','applying');

CREATE TABLE apply_steps (
    plan_id TEXT NOT NULL,
    step_index INTEGER NOT NULL CHECK(step_index >= 0),
    cursor INTEGER NOT NULL CHECK(cursor > 0),
    operation_id TEXT NOT NULL,
    object_id TEXT NOT NULL,
    mutation TEXT NOT NULL CHECK(mutation IN ('create','update','move','delete')),
    object_type TEXT NOT NULL CHECK(object_type IN ('note','folder')),
    revision INTEGER NOT NULL CHECK(revision > 0),
    parent_id TEXT,
    name TEXT NOT NULL,
    blob_hash BLOB,
    state TEXT NOT NULL CHECK(state IN ('pending','applied')),
    PRIMARY KEY(plan_id,step_index),
    FOREIGN KEY(plan_id) REFERENCES apply_plans(plan_id) ON DELETE CASCADE,
    CHECK(blob_hash IS NULL OR (typeof(blob_hash)='blob' AND length(blob_hash)=32))
);
