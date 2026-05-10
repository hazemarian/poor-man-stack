-- 0001_init.sql — bootstrap schema for pmcluster.
--
-- Note: the schema_version table is created by the migration runner
-- (internal/store/migrations.go) before any migration runs, so it does NOT
-- belong here.
--
-- Tables added in later migrations:
--   0002_credentials.sql  (Phase 2)  — managed_credentials
--   0003_stacks.sql       (Phase 3)  — stacks, stack_revisions
--   0004_secrets.sql      (Phase 4)  — registries, webhook_secrets
--   0005_backups.sql      (Phase 5)  — backups

CREATE TABLE users (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    token_hash TEXT    NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;
