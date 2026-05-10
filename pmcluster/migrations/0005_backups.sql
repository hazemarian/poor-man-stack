-- 0005_backups.sql
--
-- Tracks on-demand and pre-deploy backup runs triggered by pmcluster.
-- The actual archives live on the host at /var/backups/docker-volumes/
-- (managed by the offen/docker-volume-backup service); this table is
-- the audit log of when and why we triggered them.

CREATE TABLE backups (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    -- For on-demand backups stack_name and revision are NULL; for
    -- pre-deploy backups they identify which deploy triggered the run.
    stack_name    TEXT,
    revision      INTEGER,
    -- "pending" while the offen exec is running; final state is
    -- "succeeded" or "failed". We keep the row in either case so
    -- operators can grep failures.
    status        TEXT NOT NULL,
    -- Comma-separated list of archive paths reported by offen on stdout.
    -- Empty string when status != "succeeded".
    archive_paths TEXT NOT NULL DEFAULT '',
    error_message TEXT NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER
) STRICT;

CREATE INDEX idx_backups_stack ON backups(stack_name, revision);
CREATE INDEX idx_backups_started ON backups(started_at DESC);
