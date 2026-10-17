-- observability module: data sources, alerts, notification channels
CREATE TABLE IF NOT EXISTS obs_datasources (
    id                 BIGSERIAL PRIMARY KEY,
    name               TEXT        NOT NULL UNIQUE,
    type               TEXT        NOT NULL CHECK (type IN ('prometheus','loki','elasticsearch','alertmanager','jaeger','tempo')),
    url                TEXT        NOT NULL,
    env                TEXT        NOT NULL DEFAULT '',
    auth_type          TEXT        NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none','basic','bearer')),
    username           TEXT        NOT NULL DEFAULT '',
    secret_enc         TEXT        NOT NULL DEFAULT '',
    tls_skip_verify    BOOLEAN     NOT NULL DEFAULT FALSE,
    timeout_sec        INT         NOT NULL DEFAULT 30,
    options            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    webhook_token_hash TEXT        NOT NULL DEFAULT '',
    created_by         TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS obs_datasources_webhook_idx ON obs_datasources (webhook_token_hash) WHERE webhook_token_hash <> '';

CREATE TABLE IF NOT EXISTS obs_alerts (
    id               BIGSERIAL PRIMARY KEY,
    datasource_id    BIGINT      NOT NULL REFERENCES obs_datasources(id) ON DELETE CASCADE,
    fingerprint      TEXT        NOT NULL,
    alertname        TEXT        NOT NULL DEFAULT '',
    severity         TEXT        NOT NULL DEFAULT '',
    status           TEXT        NOT NULL CHECK (status IN ('firing','resolved')),
    handle_status    TEXT        NOT NULL DEFAULT 'open' CHECK (handle_status IN ('open','acked','closed')),
    labels           JSONB       NOT NULL DEFAULT '{}'::jsonb,
    annotations      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    generator_url    TEXT        NOT NULL DEFAULT '',
    starts_at        TIMESTAMPTZ,
    ends_at          TIMESTAMPTZ,
    fire_count       INT         NOT NULL DEFAULT 1,
    acked_by         TEXT        NOT NULL DEFAULT '',
    acked_at         TIMESTAMPTZ,
    closed_by        TEXT        NOT NULL DEFAULT '',
    closed_at        TIMESTAMPTZ,
    close_comment    TEXT        NOT NULL DEFAULT '',
    last_received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (datasource_id, fingerprint)
);
CREATE INDEX IF NOT EXISTS obs_alerts_status_idx ON obs_alerts (status, handle_status, last_received_at DESC);

CREATE TABLE IF NOT EXISTS obs_channels (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    type         TEXT        NOT NULL CHECK (type IN ('dingtalk','feishu','wecom','email')),
    config_enc   TEXT        NOT NULL,
    enabled      BOOLEAN     NOT NULL DEFAULT TRUE,
    severities   TEXT[]      NOT NULL DEFAULT '{}',
    envs         TEXT[]      NOT NULL DEFAULT '{}',
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
