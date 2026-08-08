ALTER TABLE conflict_materializations ADD COLUMN rebased_operation_id TEXT;

CREATE UNIQUE INDEX conflict_materializations_rebased_operation_unique
ON conflict_materializations(rebased_operation_id)
WHERE rebased_operation_id IS NOT NULL;

CREATE TRIGGER conflict_materializations_rebase_immutable
BEFORE UPDATE OF rebased_operation_id ON conflict_materializations
BEGIN SELECT RAISE(ABORT, 'conflict materialization rebase identity is immutable'); END;
