CREATE TABLE sync_outbox_folder_intents (
    operation_id TEXT PRIMARY KEY,
    folder_id TEXT NOT NULL,
    mutation_kind TEXT NOT NULL CHECK(mutation_kind IN ('move','delete')),
    source_relative TEXT NOT NULL,
    device INTEGER NOT NULL CHECK(device > 0),
    inode INTEGER NOT NULL CHECK(inode > 0),
    FOREIGN KEY(operation_id) REFERENCES sync_outbox(operation_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER sync_outbox_folder_intents_no_update
BEFORE UPDATE ON sync_outbox_folder_intents
BEGIN SELECT RAISE(ABORT, 'outbox folder intent is immutable'); END;
CREATE TRIGGER sync_outbox_folder_intents_no_delete
BEFORE DELETE ON sync_outbox_folder_intents
BEGIN SELECT RAISE(ABORT, 'outbox folder intent history is immutable'); END;
