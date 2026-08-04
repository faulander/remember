ALTER TABLE devices ADD COLUMN updated_at_ms INTEGER;
UPDATE devices SET
    updated_at_ms = CASE
        WHEN updated_at_ms IS NULL OR updated_at_ms < created_at_ms THEN created_at_ms
        ELSE updated_at_ms
    END,
    revoked_at_ms = CASE
        WHEN status = 'active' THEN NULL
        WHEN revoked_at_ms IS NULL OR revoked_at_ms < created_at_ms THEN created_at_ms
        ELSE revoked_at_ms
    END;

CREATE TRIGGER devices_strict_insert
BEFORE INSERT ON devices
WHEN NEW.updated_at_ms IS NULL OR NEW.updated_at_ms < NEW.created_at_ms OR
     (NEW.status = 'active' AND NEW.revoked_at_ms IS NOT NULL) OR
     (NEW.status = 'revoked' AND (NEW.revoked_at_ms IS NULL OR NEW.revoked_at_ms < NEW.created_at_ms))
BEGIN
    SELECT RAISE(ABORT, 'invalid device lifecycle');
END;

CREATE TRIGGER devices_strict_update
BEFORE UPDATE ON devices
WHEN NEW.updated_at_ms IS NULL OR NEW.updated_at_ms < NEW.created_at_ms OR
     (NEW.status = 'active' AND NEW.revoked_at_ms IS NOT NULL) OR
     (NEW.status = 'revoked' AND (NEW.revoked_at_ms IS NULL OR NEW.revoked_at_ms < NEW.created_at_ms))
BEGIN
    SELECT RAISE(ABORT, 'invalid device lifecycle');
END;

CREATE TABLE sessions (
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    id BLOB NOT NULL CHECK(typeof(id) = 'blob' AND length(id) = 16),
    device_id BLOB NOT NULL CHECK(typeof(device_id) = 'blob' AND length(device_id) = 16),
    status TEXT NOT NULL CHECK(status IN ('active', 'revoked')),
    created_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms = created_at_ms + 2592000000),
    revoked_at_ms INTEGER CHECK(revoked_at_ms IS NULL OR revoked_at_ms >= created_at_ms),
    PRIMARY KEY(user_id, id),
    UNIQUE(user_id, id, device_id),
    FOREIGN KEY(user_id, device_id) REFERENCES devices(user_id, id) ON DELETE CASCADE,
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE,
    CHECK((status = 'active' AND revoked_at_ms IS NULL) OR
          (status = 'revoked' AND revoked_at_ms IS NOT NULL))
);

CREATE TABLE access_tokens (
    token_hash BLOB PRIMARY KEY CHECK(typeof(token_hash) = 'blob' AND length(token_hash) = 32),
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    session_id BLOB NOT NULL CHECK(typeof(session_id) = 'blob' AND length(session_id) = 16),
    device_id BLOB NOT NULL CHECK(typeof(device_id) = 'blob' AND length(device_id) = 16),
    issued_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms > issued_at_ms AND expires_at_ms <= issued_at_ms + 900000),
    revoked_at_ms INTEGER CHECK(revoked_at_ms IS NULL OR revoked_at_ms >= issued_at_ms),
    FOREIGN KEY(user_id, session_id, device_id)
        REFERENCES sessions(user_id, id, device_id) ON DELETE CASCADE
);

CREATE TABLE refresh_tokens (
    token_hash BLOB PRIMARY KEY CHECK(typeof(token_hash) = 'blob' AND length(token_hash) = 32),
    user_id BLOB NOT NULL CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    session_id BLOB NOT NULL CHECK(typeof(session_id) = 'blob' AND length(session_id) = 16),
    device_id BLOB NOT NULL CHECK(typeof(device_id) = 'blob' AND length(device_id) = 16),
    replaced_by_hash BLOB UNIQUE CHECK(replaced_by_hash IS NULL OR (typeof(replaced_by_hash) = 'blob' AND length(replaced_by_hash) = 32)),
    issued_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms > issued_at_ms AND expires_at_ms <= issued_at_ms + 2592000000),
    consumed_at_ms INTEGER CHECK(consumed_at_ms IS NULL OR (consumed_at_ms >= issued_at_ms AND consumed_at_ms < expires_at_ms)),
    UNIQUE(token_hash, user_id, session_id, device_id),
    FOREIGN KEY(user_id, session_id, device_id)
        REFERENCES sessions(user_id, id, device_id) ON DELETE CASCADE,
    FOREIGN KEY(replaced_by_hash, user_id, session_id, device_id)
        REFERENCES refresh_tokens(token_hash, user_id, session_id, device_id),
    CHECK((replaced_by_hash IS NULL AND consumed_at_ms IS NULL) OR
          (replaced_by_hash IS NOT NULL AND consumed_at_ms IS NOT NULL))
);

CREATE INDEX sessions_user_status_idx ON sessions(user_id, status, created_at_ms DESC);
CREATE INDEX sessions_device_status_idx ON sessions(user_id, device_id, status);
CREATE INDEX access_tokens_session_idx ON access_tokens(user_id, session_id);
CREATE INDEX access_tokens_expiry_idx ON access_tokens(expires_at_ms);
CREATE INDEX refresh_tokens_session_idx ON refresh_tokens(user_id, session_id);
CREATE INDEX refresh_tokens_expiry_idx ON refresh_tokens(expires_at_ms);
