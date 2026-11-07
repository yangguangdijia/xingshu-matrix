-- CICD migration 9: delivery safety.
--   cicd_image_scans        : image scan results of pipeline "scan" stages
--                             (counts per severity + the top N findings).
--   cicd_webhook_deliveries : Git webhook delivery history and de-duplication
--                             by provider delivery id (kept 7 days).
--   cicd_canaries           : canary releases (Ingress-NGINX canary weight),
--   cicd_canary_events      : their step / analysis / operator timeline.

CREATE TABLE IF NOT EXISTS cicd_image_scans (
    id              BIGSERIAL PRIMARY KEY,
    run_id          BIGINT NOT NULL REFERENCES cicd_runs(id) ON DELETE CASCADE,
    stage_idx       INT NOT NULL,
    image           TEXT NOT NULL DEFAULT '',
    source          TEXT NOT NULL DEFAULT 'trivy',
    status          TEXT NOT NULL DEFAULT 'passed', -- passed|blocked|error
    critical        INT NOT NULL DEFAULT 0,
    high            INT NOT NULL DEFAULT 0,
    medium          INT NOT NULL DEFAULT 0,
    low             INT NOT NULL DEFAULT 0,
    unknown         INT NOT NULL DEFAULT 0,
    total           INT NOT NULL DEFAULT 0,
    ignored         INT NOT NULL DEFAULT 0,
    blocking        INT NOT NULL DEFAULT 0,
    policy          JSONB NOT NULL DEFAULT '{}',
    vulnerabilities JSONB NOT NULL DEFAULT '[]',
    error           TEXT NOT NULL DEFAULT '',
    scanned_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cicd_image_scans_run_idx ON cicd_image_scans (run_id, stage_idx);

CREATE TABLE IF NOT EXISTS cicd_webhook_deliveries (
    id                BIGSERIAL PRIMARY KEY,
    app_id            BIGINT NOT NULL REFERENCES cicd_applications(id) ON DELETE CASCADE,
    provider          TEXT NOT NULL,
    delivery_id       TEXT,           -- NULL when the provider sent none (not de-duplicated)
    event             TEXT NOT NULL DEFAULT '',
    ref               TEXT NOT NULL DEFAULT '',
    commit_sha        TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'processing', -- processing|triggered|ignored|error
    detail            TEXT NOT NULL DEFAULT '',
    runs              JSONB NOT NULL DEFAULT '[]',        -- [{"pipeline_id":1,"run_id":2}]
    duplicates        INT NOT NULL DEFAULT 0,
    attempts          INT NOT NULL DEFAULT 1,
    received_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_duplicate_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS cicd_webhook_deliveries_uniq ON cicd_webhook_deliveries (app_id, provider, delivery_id);
CREATE INDEX IF NOT EXISTS cicd_webhook_deliveries_app_idx ON cicd_webhook_deliveries (app_id, id DESC);
CREATE INDEX IF NOT EXISTS cicd_webhook_deliveries_received_idx ON cicd_webhook_deliveries (received_at);

CREATE TABLE IF NOT EXISTS cicd_canaries (
    id              BIGSERIAL PRIMARY KEY,
    app_id          BIGINT NOT NULL REFERENCES cicd_applications(id) ON DELETE CASCADE,
    env             TEXT NOT NULL,
    target_id       BIGINT NOT NULL,
    target_config   TEXT NOT NULL DEFAULT '',
    release_id      BIGINT,
    image           TEXT NOT NULL,
    stable_image    TEXT NOT NULL DEFAULT '',
    namespace       TEXT NOT NULL,
    deployment      TEXT NOT NULL,
    container       TEXT NOT NULL,
    service         TEXT NOT NULL,
    ingress         TEXT NOT NULL,
    plan            JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'draft',
    -- draft|awaiting_approval|pending|starting|progressing|paused|promoting|rolling_back
    -- |succeeded|rolled_back|failed|rejected|cancelled
    current_step    INT NOT NULL DEFAULT -1,
    weight          INT NOT NULL DEFAULT 0,
    step_acked      BOOLEAN NOT NULL DEFAULT false,
    step_started_at TIMESTAMPTZ,
    last_check_at   TIMESTAMPTZ,
    last_check_ok   BOOLEAN,
    check_failures  INT NOT NULL DEFAULT 0,
    next_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts        INT NOT NULL DEFAULT 0,
    message         TEXT NOT NULL DEFAULT '',
    approval_id     BIGINT,
    reason          TEXT NOT NULL DEFAULT '',
    created_by      TEXT NOT NULL DEFAULT '',
    created_by_id   BIGINT NOT NULL DEFAULT 0,
    lease_owner     TEXT NOT NULL DEFAULT '',
    lease_until     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS cicd_canaries_app_idx ON cicd_canaries (app_id, env, id DESC);
CREATE INDEX IF NOT EXISTS cicd_canaries_due_idx ON cicd_canaries (status, next_at);
-- One unfinished canary per application environment.
CREATE UNIQUE INDEX IF NOT EXISTS cicd_canaries_active_uniq ON cicd_canaries (app_id, env)
    WHERE status NOT IN ('succeeded','rolled_back','failed','rejected','cancelled');

CREATE TABLE IF NOT EXISTS cicd_canary_events (
    id         BIGSERIAL PRIMARY KEY,
    canary_id  BIGINT NOT NULL REFERENCES cicd_canaries(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL, -- info|step|analysis|operator|error
    message    TEXT NOT NULL DEFAULT '',
    data       JSONB NOT NULL DEFAULT '{}',
    actor      TEXT NOT NULL DEFAULT '',
    ts         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cicd_canary_events_idx ON cicd_canary_events (canary_id, id);
