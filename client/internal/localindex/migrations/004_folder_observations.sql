ALTER TABLE objects ADD COLUMN folder_device INTEGER CHECK(folder_device IS NULL OR folder_device >= 0);
ALTER TABLE objects ADD COLUMN folder_inode INTEGER CHECK(folder_inode IS NULL OR folder_inode >= 0);

CREATE TABLE apply_folder_mutations (
    plan_id TEXT NOT NULL,
    step_index INTEGER NOT NULL CHECK(step_index >= 0),
    folder_id TEXT NOT NULL,
    mutation_kind TEXT NOT NULL CHECK(mutation_kind IN ('move','delete')),
    source_relative TEXT NOT NULL,
    target_relative TEXT NOT NULL,
    device INTEGER NOT NULL CHECK(device >= 0),
    inode INTEGER NOT NULL CHECK(inode >= 0),
    PRIMARY KEY(plan_id, step_index),
    FOREIGN KEY(plan_id, step_index) REFERENCES apply_steps(plan_id, step_index) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TRIGGER apply_folder_mutations_immutable
BEFORE UPDATE ON apply_folder_mutations
BEGIN SELECT RAISE(ABORT, 'folder mutation binding is immutable'); END;
