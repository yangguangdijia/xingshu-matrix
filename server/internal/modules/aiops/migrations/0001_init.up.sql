-- aiops: AI 运维工作台（v1.33）。PostgreSQL（与仓库其他模块迁移一致）。
-- 自动处置策略（policy）存放在 settings（Deps.Settings），不建表。

-- 步骤 ID 全局唯一（steps 以 JSONB 存在 aiops_plans.steps 中，ID 由此序列分配）。
CREATE SEQUENCE IF NOT EXISTS aiops_step_id_seq;

CREATE TABLE IF NOT EXISTS aiops_cases (
    id           BIGSERIAL PRIMARY KEY,
    kind         TEXT NOT NULL DEFAULT 'request',      -- alert | deploy | request
    source       TEXT NOT NULL DEFAULT 'manual',       -- alert | chat | manual | auto
    title        TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'investigating',
    env          TEXT NOT NULL DEFAULT '',
    cluster_id   BIGINT,
    alert_id     BIGINT,
    asset_id     BIGINT,
    request      TEXT NOT NULL DEFAULT '',             -- 不含凭据
    target       JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence     JSONB NOT NULL DEFAULT '[]'::jsonb,   -- []EvidenceItem
    conclusion   JSONB NOT NULL DEFAULT '{}'::jsonb,   -- Conclusion
    mode         TEXT NOT NULL DEFAULT '',             -- ai | manual | mixed
    plan_id      BIGINT,                               -- 已采纳方案
    approval_id  BIGINT,
    runbook_id   BIGINT,
    engine       TEXT NOT NULL DEFAULT '',             -- model | rules
    error        TEXT NOT NULL DEFAULT '',
    created_by   BIGINT NOT NULL DEFAULT 0,
    created_name TEXT NOT NULL DEFAULT '',
    reviewed_by  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS aiops_cases_status_idx  ON aiops_cases (status);
CREATE INDEX IF NOT EXISTS aiops_cases_created_idx ON aiops_cases (created_at DESC);
CREATE INDEX IF NOT EXISTS aiops_cases_alert_idx   ON aiops_cases (alert_id) WHERE alert_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS aiops_plans (
    id           BIGSERIAL PRIMARY KEY,
    case_id      BIGINT NOT NULL REFERENCES aiops_cases(id) ON DELETE CASCADE,
    seq          INT  NOT NULL DEFAULT 0,
    title        TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    risk         TEXT NOT NULL DEFAULT 'read',          -- read | change | high
    impact       TEXT NOT NULL DEFAULT '',
    estimate_sec INT  NOT NULL DEFAULT 0,
    rollback     TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'proposed',      -- proposed | accepted | rejected | superseded
    reject_note  TEXT NOT NULL DEFAULT '',
    steps        JSONB NOT NULL DEFAULT '[]'::jsonb,    -- []Step（含 phase=verify/rollback）
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS aiops_plans_case_idx ON aiops_plans (case_id, seq);

CREATE TABLE IF NOT EXISTS aiops_timeline (
    id         BIGSERIAL PRIMARY KEY,
    case_id    BIGINT NOT NULL REFERENCES aiops_cases(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,                           -- created | evidence | concluded | review | step | approval | ...
    actor      TEXT NOT NULL DEFAULT '',                -- 用户名 / "ai" / "system"
    detail     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS aiops_timeline_case_idx ON aiops_timeline (case_id, id);

CREATE TABLE IF NOT EXISTS aiops_runbooks (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    match        JSONB NOT NULL DEFAULT '{}'::jsonb,
    plan         JSONB NOT NULL DEFAULT '{}'::jsonb,    -- PlanDraft
    risk         TEXT NOT NULL DEFAULT 'read',
    uses         INT  NOT NULL DEFAULT 0,
    successes    INT  NOT NULL DEFAULT 0,
    auto_execute BOOLEAN NOT NULL DEFAULT false,        -- 仅 risk != high 时允许
    auto_scope   JSONB NOT NULL DEFAULT '{}'::jsonb,
    source_case  BIGINT,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS aiops_runbooks_auto_idx ON aiops_runbooks (auto_execute) WHERE auto_execute;
