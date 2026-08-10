CREATE TABLE sync_integrity_incidents (
    incident_id INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id TEXT NOT NULL,
    cursor INTEGER NOT NULL CHECK(cursor > 0),
    object_id TEXT NOT NULL,
    blob_hash BLOB NOT NULL CHECK(typeof(blob_hash)='blob' AND length(blob_hash)=32),
    code TEXT NOT NULL CHECK(code IN ('missing_blob','hash_mismatch')),
    first_detected_at_ms INTEGER NOT NULL,
    last_detected_at_ms INTEGER NOT NULL,
    occurrence_count INTEGER NOT NULL CHECK(occurrence_count > 0),
    acknowledged_at_ms INTEGER,
    UNIQUE(cursor, code),
    FOREIGN KEY(plan_id) REFERENCES apply_plans(plan_id),
    FOREIGN KEY(cursor) REFERENCES sync_inbox_changes(cursor)
);
CREATE TRIGGER sync_integrity_incident_insert_guard BEFORE INSERT ON sync_integrity_incidents WHEN NOT EXISTS(SELECT 1 FROM apply_steps s JOIN sync_inbox_changes i ON i.cursor=s.cursor WHERE s.plan_id=NEW.plan_id AND s.cursor=NEW.cursor AND s.object_id=NEW.object_id AND s.blob_hash=NEW.blob_hash AND i.object_id=s.object_id AND i.blob_hash=s.blob_hash) BEGIN SELECT RAISE(ABORT,'invalid integrity incident binding'); END;
CREATE INDEX sync_integrity_incidents_open_idx ON sync_integrity_incidents(acknowledged_at_ms, last_detected_at_ms);
CREATE TRIGGER sync_integrity_incident_binding_immutable BEFORE UPDATE OF plan_id,cursor,object_id,blob_hash,code,first_detected_at_ms ON sync_integrity_incidents BEGIN SELECT RAISE(ABORT,'integrity incident binding is immutable'); END;
CREATE TRIGGER sync_integrity_incident_progress_guard BEFORE UPDATE OF last_detected_at_ms,occurrence_count,acknowledged_at_ms ON sync_integrity_incidents WHEN NOT((NEW.occurrence_count=OLD.occurrence_count+1 AND NEW.last_detected_at_ms>=OLD.last_detected_at_ms AND NEW.acknowledged_at_ms IS NULL) OR (NEW.occurrence_count=OLD.occurrence_count AND NEW.last_detected_at_ms=OLD.last_detected_at_ms AND OLD.acknowledged_at_ms IS NULL AND NEW.acknowledged_at_ms IS NOT NULL)) BEGIN SELECT RAISE(ABORT,'invalid integrity incident transition'); END;
CREATE TRIGGER sync_integrity_incident_no_delete BEFORE DELETE ON sync_integrity_incidents BEGIN SELECT RAISE(ABORT,'integrity incident is durable'); END;
CREATE TRIGGER sync_integrity_incident_step_binding_immutable BEFORE UPDATE OF plan_id,cursor,object_id,blob_hash ON apply_steps WHEN EXISTS(SELECT 1 FROM sync_integrity_incidents incident WHERE incident.plan_id=OLD.plan_id AND incident.cursor=OLD.cursor AND incident.object_id=OLD.object_id AND incident.blob_hash=OLD.blob_hash) BEGIN SELECT RAISE(ABORT,'integrity incident apply step binding is immutable'); END;
CREATE TRIGGER sync_integrity_incident_step_no_delete BEFORE DELETE ON apply_steps WHEN EXISTS(SELECT 1 FROM sync_integrity_incidents incident WHERE incident.plan_id=OLD.plan_id AND incident.cursor=OLD.cursor AND incident.object_id=OLD.object_id AND incident.blob_hash=OLD.blob_hash) BEGIN SELECT RAISE(ABORT,'integrity incident apply step binding is immutable'); END;
