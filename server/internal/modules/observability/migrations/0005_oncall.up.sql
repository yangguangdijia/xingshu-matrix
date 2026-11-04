-- 值班与故障（PagerDuty / 夜莺 风格）：分层轮值排班、代班、故障声明与时间线、SLO 与错误预算。
CREATE TABLE IF NOT EXISTS oncall_schedules (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT        NOT NULL UNIQUE,
    description TEXT        NOT NULL DEFAULT '',
    env         TEXT        NOT NULL DEFAULT '',
    timezone    TEXT        NOT NULL DEFAULT 'Asia/Shanghai',
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS oncall_rotations (
    schedule_id    BIGINT      NOT NULL REFERENCES oncall_schedules(id) ON DELETE CASCADE,
    layer          INT         NOT NULL CHECK (layer BETWEEN 1 AND 9),
    name           TEXT        NOT NULL DEFAULT '',
    start_at       TIMESTAMPTZ NOT NULL,
    length_seconds INT         NOT NULL CHECK (length_seconds >= 3600),
    members        TEXT[]      NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (schedule_id, layer)
);
CREATE TABLE IF NOT EXISTS oncall_overrides (
    id          BIGSERIAL PRIMARY KEY,
    schedule_id BIGINT      NOT NULL REFERENCES oncall_schedules(id) ON DELETE CASCADE,
    layer       INT         NOT NULL CHECK (layer BETWEEN 1 AND 9),
    username    TEXT        NOT NULL,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL CHECK (ends_at > starts_at),
    reason      TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS oncall_overrides_window ON oncall_overrides (schedule_id, layer, starts_at, ends_at);
CREATE TABLE IF NOT EXISTS oncall_incidents (
    id           BIGSERIAL PRIMARY KEY,
    title        TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    severity     TEXT        NOT NULL DEFAULT 'P3' CHECK (severity IN ('P1','P2','P3','P4')),
    status       TEXT        NOT NULL DEFAULT 'declared' CHECK (status IN ('declared','acknowledged','resolved')),
    service      TEXT        NOT NULL DEFAULT '',
    env          TEXT        NOT NULL DEFAULT '',
    schedule_id  BIGINT      REFERENCES oncall_schedules(id) ON DELETE SET NULL,
    assigned_to  TEXT        NOT NULL DEFAULT '',
    declared_by  TEXT        NOT NULL DEFAULT '',
    declared_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    acked_by     TEXT        NOT NULL DEFAULT '',
    acked_at     TIMESTAMPTZ,
    resolved_by  TEXT        NOT NULL DEFAULT '',
    resolved_at  TIMESTAMPTZ,
    resolve_note TEXT        NOT NULL DEFAULT '',
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS oncall_incidents_status ON oncall_incidents (status, declared_at DESC);
CREATE TABLE IF NOT EXISTS oncall_incident_events (
    id          BIGSERIAL PRIMARY KEY,
    incident_id BIGINT      NOT NULL REFERENCES oncall_incidents(id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL,
    actor       TEXT        NOT NULL DEFAULT '',
    message     TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS oncall_incident_events_inc ON oncall_incident_events (incident_id, id);
CREATE TABLE IF NOT EXISTS oncall_slos (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT          NOT NULL UNIQUE,
    service       TEXT          NOT NULL DEFAULT '',
    env           TEXT          NOT NULL DEFAULT '',
    description   TEXT          NOT NULL DEFAULT '',
    target        NUMERIC(7,4)  NOT NULL CHECK (target > 0 AND target <= 100),
    window_days   INT           NOT NULL DEFAULT 30 CHECK (window_days BETWEEN 1 AND 365),
    source        TEXT          NOT NULL DEFAULT 'manual' CHECK (source IN ('manual','prometheus')),
    sli_type      TEXT          NOT NULL DEFAULT 'ratio' CHECK (sli_type IN ('ratio','query')),
    datasource_id BIGINT        REFERENCES obs_datasources(id) ON DELETE SET NULL,
    good_query    TEXT          NOT NULL DEFAULT '',
    total_query   TEXT          NOT NULL DEFAULT '',
    query         TEXT          NOT NULL DEFAULT '',
    created_by    TEXT          NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ   NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS oncall_slo_entries (
    id           BIGSERIAL PRIMARY KEY,
    slo_id       BIGINT      NOT NULL REFERENCES oncall_slos(id) ON DELETE CASCADE,
    period_start TIMESTAMPTZ NOT NULL,
    period_end   TIMESTAMPTZ NOT NULL CHECK (period_end > period_start),
    good         BIGINT      NOT NULL CHECK (good >= 0),
    total        BIGINT      NOT NULL CHECK (total > 0 AND good <= total),
    note         TEXT        NOT NULL DEFAULT '',
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS oncall_slo_entries_slo ON oncall_slo_entries (slo_id, period_end);
