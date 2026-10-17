CREATE TABLE IF NOT EXISTS middleware_instances (
    id           BIGSERIAL PRIMARY KEY,
    name         VARCHAR(128) NOT NULL UNIQUE,
    type         VARCHAR(16)  NOT NULL,
    env          VARCHAR(64)  NOT NULL DEFAULT '',
    endpoints    JSONB        NOT NULL DEFAULT '[]',
    auth_enc     TEXT         NOT NULL DEFAULT '',
    options      JSONB        NOT NULL DEFAULT '{}',
    description  TEXT         NOT NULL DEFAULT '',
    created_by   VARCHAR(128) NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_type ON middleware_instances(type);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_env ON middleware_instances(env);

-- every SQL / Redis console statement (executed, failed, blocked or sent to approval)
CREATE TABLE IF NOT EXISTS middleware_sql_audit (
    id            BIGSERIAL PRIMARY KEY,
    instance_id   BIGINT       NOT NULL,
    instance_name VARCHAR(128) NOT NULL DEFAULT '',
    instance_type VARCHAR(16)  NOT NULL DEFAULT '',
    env           VARCHAR(64)  NOT NULL DEFAULT '',
    database_name VARCHAR(256) NOT NULL DEFAULT '',
    statement     TEXT         NOT NULL,
    risk          VARCHAR(16)  NOT NULL DEFAULT '',
    status        VARCHAR(16)  NOT NULL,
    error         TEXT         NOT NULL DEFAULT '',
    rows_count    BIGINT       NOT NULL DEFAULT 0,
    duration_ms   BIGINT       NOT NULL DEFAULT 0,
    approval_id   BIGINT,
    user_id       BIGINT       NOT NULL DEFAULT 0,
    username      VARCHAR(128) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_middleware_sql_audit_instance ON middleware_sql_audit(instance_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_middleware_sql_audit_created ON middleware_sql_audit(created_at DESC);
