CREATE TABLE conflict_folder_publications (
    folder_id TEXT PRIMARY KEY,
    target_relative TEXT NOT NULL UNIQUE,
    stage_relative TEXT NOT NULL UNIQUE,
    nonce BLOB NOT NULL CHECK(typeof(nonce)='blob' AND length(nonce)=32),
    device INTEGER NOT NULL CHECK(device >= 0),
    inode INTEGER NOT NULL CHECK(inode >= 0),
    state TEXT NOT NULL CHECK(state IN ('prepared','published','cleaned'))
) WITHOUT ROWID;

CREATE TABLE conflict_materializations (
    operation_id TEXT PRIMARY KEY,
    source_object_id TEXT NOT NULL,
    conflict_note_id TEXT NOT NULL UNIQUE,
    original_relative TEXT NOT NULL,
    target_relative TEXT NOT NULL UNIQUE,
    source_hash BLOB NOT NULL CHECK(typeof(source_hash)='blob' AND length(source_hash)=32),
    materialized_hash BLOB NOT NULL CHECK(typeof(materialized_hash)='blob' AND length(materialized_hash)=32),
    staged_relative TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK(state IN ('prepared','copy_staged','copy_published','completed')),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER conflict_folder_publications_identity_immutable
BEFORE UPDATE OF folder_id,target_relative,stage_relative,nonce,device,inode ON conflict_folder_publications
BEGIN SELECT RAISE(ABORT, 'conflict folder publication identity is immutable'); END;
CREATE TRIGGER conflict_folder_publications_state_monotonic
BEFORE UPDATE OF state ON conflict_folder_publications
WHEN (OLD.state='published' AND NEW.state='prepared') OR (OLD.state='cleaned' AND NEW.state<>'cleaned')
BEGIN SELECT RAISE(ABORT, 'conflict folder publication state is monotonic'); END;

CREATE TRIGGER conflict_materializations_identity_immutable
BEFORE UPDATE OF operation_id,source_object_id,conflict_note_id,original_relative,target_relative,source_hash,materialized_hash,staged_relative ON conflict_materializations
BEGIN SELECT RAISE(ABORT, 'conflict materialization identity is immutable'); END;
CREATE TRIGGER conflict_materializations_state_monotonic
BEFORE UPDATE OF state ON conflict_materializations
WHEN (OLD.state='copy_staged' AND NEW.state='prepared') OR (OLD.state='copy_published' AND NEW.state IN ('prepared','copy_staged')) OR (OLD.state='completed' AND NEW.state<>'completed')
BEGIN SELECT RAISE(ABORT, 'conflict materialization state is monotonic'); END;
