ALTER TABLE conflict_materializations ADD COLUMN cleaned_at_ms INTEGER;

CREATE TRIGGER conflict_materializations_cleanup_guard
BEFORE UPDATE OF cleaned_at_ms ON conflict_materializations
WHEN (NEW.cleaned_at_ms IS NOT NULL AND OLD.state<>'completed')
  OR (OLD.cleaned_at_ms IS NOT NULL AND NEW.cleaned_at_ms IS NOT OLD.cleaned_at_ms)
BEGIN SELECT RAISE(ABORT, 'conflict materialization cleanup is completion-only and immutable'); END;
