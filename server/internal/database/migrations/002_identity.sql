CREATE TABLE users (
    id BLOB PRIMARY KEY CHECK(typeof(id) = 'blob' AND length(id) = 16),
    email_delivery TEXT NOT NULL,
    email_canonical TEXT NOT NULL COLLATE BINARY UNIQUE,
    password_hash TEXT NOT NULL,
    password_policy INTEGER NOT NULL CHECK(password_policy > 0),
    status TEXT NOT NULL CHECK(status IN ('pending_verification', 'active', 'deletion_pending')),
    created_at_ms INTEGER NOT NULL,
    verified_at_ms INTEGER,
    deletion_requested_at_ms INTEGER
);

CREATE TABLE email_verifications (
    user_id BLOB PRIMARY KEY CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    token_hash BLOB NOT NULL UNIQUE CHECK(typeof(token_hash) = 'blob' AND length(token_hash) = 32),
    issued_at_ms INTEGER NOT NULL,
    expires_at_ms INTEGER NOT NULL CHECK(expires_at_ms > issued_at_ms),
    FOREIGN KEY(user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX users_status_idx ON users(status);
CREATE INDEX email_verifications_expiry_idx ON email_verifications(expires_at_ms);
