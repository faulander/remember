CREATE TABLE server_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO server_metadata(key, value) VALUES ('schema_generation', 'foundation');
