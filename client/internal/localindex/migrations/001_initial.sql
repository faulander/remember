CREATE TABLE objects (
    object_id TEXT PRIMARY KEY,
    object_type TEXT NOT NULL CHECK (object_type IN ('note', 'folder')),
    relative_path TEXT NOT NULL UNIQUE,
    collision_path TEXT NOT NULL,
    parent_id TEXT,
    content_hash BLOB,
    identity_state TEXT NOT NULL CHECK (identity_state IN ('known', 'new', 'pending'))
);

CREATE TABLE local_issues (
    issue_id INTEGER PRIMARY KEY,
    code TEXT NOT NULL,
    relative_path TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT ''
);

CREATE INDEX objects_collision_path_idx ON objects(collision_path);
CREATE INDEX local_issues_path_idx ON local_issues(relative_path, code);

CREATE TABLE watcher_state (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
