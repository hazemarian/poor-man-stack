-- 0003_stacks.sql — application stacks deployed via pmcluster (Phase 3).
--
-- Each `stacks` row is a logical app (donation-campaign, etc.). Every
-- deploy adds a row to `stack_revisions` keyed by a unix-timestamp
-- revision id; the parent stacks row tracks which revision is currently
-- live. Rollback re-applies a stored revision as a NEW revision (preserves
-- audit trail and lets us tell apart "deployed" vs "rolled back from").
--
-- source_yaml   — the literal DSL the operator submitted
-- rendered_yaml — the compose YAML pmcluster produced from it
-- payload_json  — the full webhook/API payload as JSON (app_name, repo_url,
--                 version, ...) for audit/UI display

CREATE TABLE stacks (
    name             TEXT    PRIMARY KEY,
    current_revision INTEGER NOT NULL,
    repo_url         TEXT,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
) STRICT;

CREATE TABLE stack_revisions (
    stack_name    TEXT    NOT NULL,
    revision      INTEGER NOT NULL,
    source_yaml   TEXT    NOT NULL,
    rendered_yaml TEXT    NOT NULL,
    payload_json  TEXT,
    created_at    INTEGER NOT NULL,
    PRIMARY KEY (stack_name, revision),
    FOREIGN KEY (stack_name) REFERENCES stacks(name) ON DELETE CASCADE
) STRICT;

CREATE INDEX idx_stack_revisions_stack ON stack_revisions(stack_name, revision DESC);
