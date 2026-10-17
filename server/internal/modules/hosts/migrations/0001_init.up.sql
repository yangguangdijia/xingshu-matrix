CREATE TABLE IF NOT EXISTS hosts_hosts (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT        NOT NULL,
    ip            TEXT        NOT NULL DEFAULT '',
    env           TEXT        NOT NULL DEFAULT '',
    labels        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    facts         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    agent_version TEXT        NOT NULL DEFAULT '',
    secret_hash   TEXT        NOT NULL DEFAULT '',
    online        BOOLEAN     NOT NULL DEFAULT FALSE,
    last_seen_at  TIMESTAMPTZ,
    registered_at TIMESTAMPTZ,
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS hosts_hosts_env_idx ON hosts_hosts (env);

CREATE TABLE IF NOT EXISTS hosts_enroll_tokens (
    id           BIGSERIAL PRIMARY KEY,
    token_hash   TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL DEFAULT '',
    env          TEXT        NOT NULL DEFAULT '',
    labels       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    expires_at   TIMESTAMPTZ NOT NULL,
    used_at      TIMESTAMPTZ,
    used_host_id BIGINT REFERENCES hosts_hosts(id) ON DELETE SET NULL,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS hosts_tasks (
    id           BIGSERIAL PRIMARY KEY,
    host_id      BIGINT      NOT NULL REFERENCES hosts_hosts(id) ON DELETE CASCADE,
    kind         TEXT        NOT NULL DEFAULT 'exec',   -- exec | file_push
    batch_id     TEXT        NOT NULL DEFAULT '',
    command      TEXT        NOT NULL,                  -- command, or target path for file_push
    timeout_sec  INT         NOT NULL DEFAULT 60,
    status       TEXT        NOT NULL DEFAULT 'running', -- running | success | failed | timeout
    exit_code    INT,
    output       TEXT        NOT NULL DEFAULT '',
    error        TEXT        NOT NULL DEFAULT '',
    triggered_by TEXT        NOT NULL DEFAULT '',
    ticket_id    BIGINT,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS hosts_tasks_host_idx ON hosts_tasks (host_id, id DESC);
CREATE INDEX IF NOT EXISTS hosts_tasks_batch_idx ON hosts_tasks (batch_id) WHERE batch_id <> '';
