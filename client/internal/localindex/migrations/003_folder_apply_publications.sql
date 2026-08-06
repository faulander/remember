CREATE TABLE apply_folder_publications (
    plan_id TEXT NOT NULL,
    step_index INTEGER NOT NULL CHECK(step_index >= 0),
    folder_id TEXT NOT NULL,
    target_relative TEXT NOT NULL,
    stage_relative TEXT NOT NULL,
    nonce BLOB NOT NULL CHECK(typeof(nonce)='blob' AND length(nonce)=32),
    device INTEGER NOT NULL CHECK(device >= 0),
    inode INTEGER NOT NULL CHECK(inode >= 0),
    cleanup_authorized INTEGER NOT NULL DEFAULT 0 CHECK(cleanup_authorized IN (0,1)),
    cleaned_at_ms INTEGER,
    PRIMARY KEY(plan_id,step_index),
    FOREIGN KEY(plan_id,step_index) REFERENCES apply_steps(plan_id,step_index) ON DELETE CASCADE
);
CREATE UNIQUE INDEX apply_folder_publications_stage ON apply_folder_publications(stage_relative);
CREATE TRIGGER apply_folder_publications_identity_immutable
BEFORE UPDATE OF plan_id,step_index,folder_id,target_relative,stage_relative,nonce,device,inode
ON apply_folder_publications BEGIN SELECT RAISE(ABORT, 'folder publication identity is immutable'); END;
CREATE TRIGGER apply_folder_publications_cleanup_monotonic
BEFORE UPDATE OF cleanup_authorized ON apply_folder_publications
WHEN OLD.cleanup_authorized=1 AND NEW.cleanup_authorized<>1
BEGIN SELECT RAISE(ABORT, 'folder publication cleanup authorization is monotonic'); END;
CREATE TRIGGER apply_folder_publications_cleaned_immutable
BEFORE UPDATE OF cleaned_at_ms ON apply_folder_publications
WHEN OLD.cleaned_at_ms IS NOT NULL AND NEW.cleaned_at_ms IS NOT OLD.cleaned_at_ms
BEGIN SELECT RAISE(ABORT, 'folder publication cleanup completion is immutable'); END;
