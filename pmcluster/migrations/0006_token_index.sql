-- 0006_token_index.sql — add token_id as a fast lookup index to avoid
-- O(N) argon2id scanning.  New tokens embed the hex-encoded token_id as a
-- public prefix; existing rows get a NULL token_id and still work via the
-- old O(N) fallback.

ALTER TABLE users ADD COLUMN token_id TEXT;
CREATE UNIQUE INDEX idx_users_token_id ON users(token_id);
