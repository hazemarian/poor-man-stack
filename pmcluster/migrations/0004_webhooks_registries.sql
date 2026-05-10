-- 0004_webhooks_registries.sql — webhook secrets + registry credentials.
--
-- webhook_sources: one row per CI/automation source that POSTs to /webhook/{source}.
--   secret_ciphertext is HMAC-shared-secret (NOT a hash); pmcluster needs to
--   compute HMAC over each request body to verify the signature, so we store
--   the secret AES-GCM-encrypted via internal/credentials.Cipher.
--
-- registries: per-host credentials for `docker login`. password_ciphertext is
--   AES-GCM-encrypted. After `pmcluster registry add`, pmcluster ALSO writes
--   to ~/.docker/config.json so `docker stack deploy --with-registry-auth`
--   can forward auth to workers.

CREATE TABLE webhook_sources (
    source             TEXT    PRIMARY KEY,
    secret_ciphertext  BLOB    NOT NULL,
    description        TEXT,
    created_at         INTEGER NOT NULL,
    last_used_at       INTEGER
) STRICT;

CREATE TABLE registries (
    host                TEXT    PRIMARY KEY,
    username            TEXT    NOT NULL,
    password_ciphertext BLOB    NOT NULL,
    created_at          INTEGER NOT NULL
) STRICT;
