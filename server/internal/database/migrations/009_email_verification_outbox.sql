CREATE TABLE email_verification_outbox (
    user_id BLOB PRIMARY KEY CHECK(typeof(user_id) = 'blob' AND length(user_id) = 16),
    recipient TEXT NOT NULL CHECK(length(recipient) BETWEEN 3 AND 254),
    token_nonce BLOB NOT NULL CHECK(typeof(token_nonce) = 'blob' AND length(token_nonce) = 12),
    token_ciphertext BLOB NOT NULL CHECK(typeof(token_ciphertext) = 'blob' AND length(token_ciphertext) = 48),
    created_at_ms INTEGER NOT NULL,
    next_attempt_at_ms INTEGER NOT NULL CHECK(next_attempt_at_ms >= created_at_ms),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count >= 0),
    FOREIGN KEY(user_id) REFERENCES email_verifications(user_id) ON DELETE CASCADE
);

CREATE INDEX email_verification_outbox_due_idx
    ON email_verification_outbox(next_attempt_at_ms, created_at_ms);
