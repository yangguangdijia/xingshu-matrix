-- cicd: runner targets, Jenkins servers, applications, pipelines, runs, logs, releases.
CREATE TABLE IF NOT EXISTS cicd_targets (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    kubeconfig_enc TEXT NOT NULL,
    namespace      TEXT NOT NULL DEFAULT 'matrix-cicd',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cicd_jenkins (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    url        TEXT NOT NULL,
    username   TEXT NOT NULL DEFAULT '',
    token_enc  TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cicd_applications (
    id                 BIGSERIAL PRIMARY KEY,
    name               TEXT NOT NULL UNIQUE,
    repo_url           TEXT NOT NULL,
    branch             TEXT NOT NULL DEFAULT 'main',
    dockerfile         TEXT NOT NULL DEFAULT 'Dockerfile',
    context_dir        TEXT NOT NULL DEFAULT '.',
    image_repo         TEXT NOT NULL DEFAULT '',
    git_username       TEXT NOT NULL DEFAULT '',
    git_token_enc      TEXT NOT NULL DEFAULT '',
    webhook_secret_enc TEXT NOT NULL DEFAULT '',
    envs               JSONB NOT NULL DEFAULT '[]',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS cicd_pipelines (
    id               BIGSERIAL PRIMARY KEY,
    app_id           BIGINT NOT NULL REFERENCES cicd_applications(id) ON DELETE CASCADE,
    name             TEXT NOT NULL,
    runner_target_id BIGINT NOT NULL REFERENCES cicd_targets(id),
    stages           JSONB NOT NULL DEFAULT '[]',
    on_push          BOOLEAN NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (app_id, name)
);

CREATE TABLE IF NOT EXISTS cicd_runs (
    id              BIGSERIAL PRIMARY KEY,
    pipeline_id     BIGINT NOT NULL REFERENCES cicd_pipelines(id) ON DELETE CASCADE,
    app_id          BIGINT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'queued', -- queued|running|cancelling|succeeded|failed|cancelled
    trigger         TEXT NOT NULL DEFAULT 'manual',
    triggered_by_id BIGINT NOT NULL DEFAULT 0,
    triggered_by    TEXT NOT NULL DEFAULT '',
    branch          TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    image_tag       TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS cicd_runs_status_idx ON cicd_runs (status, id);
CREATE INDEX IF NOT EXISTS cicd_runs_pipeline_idx ON cicd_runs (pipeline_id, id DESC);

CREATE TABLE IF NOT EXISTS cicd_run_stages (
    run_id      BIGINT NOT NULL REFERENCES cicd_runs(id) ON DELETE CASCADE,
    idx         INT NOT NULL,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending', -- pending|running|awaiting_approval|approved|rejected|succeeded|failed|skipped|cancelled
    job_name    TEXT NOT NULL DEFAULT '',
    approval_id BIGINT,
    error       TEXT NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    PRIMARY KEY (run_id, idx)
);

CREATE TABLE IF NOT EXISTS cicd_run_logs (
    id        BIGSERIAL PRIMARY KEY,
    run_id    BIGINT NOT NULL REFERENCES cicd_runs(id) ON DELETE CASCADE,
    stage_idx INT NOT NULL,
    line      TEXT NOT NULL,
    ts        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cicd_run_logs_run_idx ON cicd_run_logs (run_id, id);

CREATE TABLE IF NOT EXISTS cicd_releases (
    id             BIGSERIAL PRIMARY KEY,
    app_id         BIGINT NOT NULL REFERENCES cicd_applications(id) ON DELETE CASCADE,
    env            TEXT NOT NULL,
    run_id         BIGINT,
    image          TEXT NOT NULL,
    previous_image TEXT NOT NULL DEFAULT '',
    kind           TEXT NOT NULL DEFAULT 'deploy', -- deploy|rollback
    status         TEXT NOT NULL DEFAULT 'running', -- running|succeeded|failed
    message        TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS cicd_releases_app_env_idx ON cicd_releases (app_id, env, id DESC);
