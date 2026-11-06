-- 命令过滤新增动作 approve（命令需审批后才执行）
ALTER TABLE bastion_filters DROP CONSTRAINT IF EXISTS bastion_filters_action_check;
ALTER TABLE bastion_filters ADD CONSTRAINT bastion_filters_action_check CHECK (action IN ('deny','confirm','approve'));

-- 改密计划：定期为托管的密码账号生成新密码、在主机上修改、用新密码验证登录后再保存。
ALTER TABLE bastion_accounts ADD COLUMN IF NOT EXISTS pending_secret_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE bastion_accounts ADD COLUMN IF NOT EXISTS password_changed_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS bastion_rotation_plans (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT        NOT NULL UNIQUE,
    account_ids   BIGINT[]    NOT NULL DEFAULT '{}',
    interval_days INT         NOT NULL DEFAULT 30 CHECK (interval_days BETWEEN 1 AND 365),
    length        INT         NOT NULL DEFAULT 20 CHECK (length BETWEEN 12 AND 64),
    enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    next_run_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_run_at   TIMESTAMPTZ,
    last_result   TEXT        NOT NULL DEFAULT '',
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS bastion_rotation_runs (
    id          BIGSERIAL PRIMARY KEY,
    plan_id     BIGINT      REFERENCES bastion_rotation_plans(id) ON DELETE CASCADE,
    account_id  BIGINT      REFERENCES bastion_accounts(id) ON DELETE SET NULL,
    asset       TEXT        NOT NULL DEFAULT '',
    username    TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL CHECK (status IN ('succeeded','failed','skipped')),
    message     TEXT        NOT NULL DEFAULT '',
    triggered_by TEXT       NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS bastion_rotation_runs_plan ON bastion_rotation_runs (plan_id, started_at DESC);
