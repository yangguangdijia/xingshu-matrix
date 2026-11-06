-- 通知中心（对标夜莺 / KubeSphere 通知管理）：
--   渠道类型（重申 CHECK：钉钉 / 飞书 / 企业微信 / 邮件 / 通用 Webhook / Slack）、渠道说明与按事件类型选择的消息模板；
--   消息模板（Go text/template）；订阅路由（事件类型 / 状态 / 级别 / 环境 / 集群 / 标签 → 渠道，静默时段、限频、聚合）；
--   发送记录（可筛选、失败可重发）；外部事件游标（故障时间线 / 流水线结果 / 审批待办）。
ALTER TABLE obs_channels DROP CONSTRAINT IF EXISTS obs_channels_type_check;
ALTER TABLE obs_channels ADD CONSTRAINT obs_channels_type_check
    CHECK (type IN ('dingtalk','feishu','wecom','email','slack','webhook'));
ALTER TABLE obs_channels ADD COLUMN IF NOT EXISTS description  TEXT        NOT NULL DEFAULT '';
ALTER TABLE obs_channels ADD COLUMN IF NOT EXISTS templates    JSONB       NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE obs_channels ADD COLUMN IF NOT EXISTS last_sent_at TIMESTAMPTZ;
ALTER TABLE obs_channels ADD COLUMN IF NOT EXISTS last_status  TEXT        NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS obs_notify_templates (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT        NOT NULL UNIQUE,
    description  TEXT        NOT NULL DEFAULT '',
    event_kind   TEXT        NOT NULL CHECK (event_kind IN ('alert','incident','pipeline','approval','test')),
    channel_type TEXT        NOT NULL DEFAULT '' CHECK (channel_type IN ('','dingtalk','feishu','wecom','email','slack','webhook')),
    title_tpl    TEXT        NOT NULL,
    body_tpl     TEXT        NOT NULL,
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS obs_notify_subscriptions (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT        NOT NULL UNIQUE,
    description   TEXT        NOT NULL DEFAULT '',
    enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    event_kinds   TEXT[]      NOT NULL DEFAULT '{alert}',
    statuses      TEXT[]      NOT NULL DEFAULT '{}',
    severities    TEXT[]      NOT NULL DEFAULT '{}',
    envs          TEXT[]      NOT NULL DEFAULT '{}',
    clusters      TEXT[]      NOT NULL DEFAULT '{}',
    match_labels  JSONB       NOT NULL DEFAULT '{}'::jsonb,
    channel_ids   BIGINT[]    NOT NULL DEFAULT '{}',
    quiet         JSONB       NOT NULL DEFAULT '{}'::jsonb,
    dedup_minutes INT         NOT NULL DEFAULT 0 CHECK (dedup_minutes BETWEEN 0 AND 1440),
    aggregate     BOOLEAN     NOT NULL DEFAULT FALSE,
    created_by    TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS obs_notify_subscriptions_channels ON obs_notify_subscriptions USING GIN (channel_ids);

CREATE TABLE IF NOT EXISTS obs_notify_records (
    id                BIGSERIAL PRIMARY KEY,
    channel_id        BIGINT      NOT NULL,
    channel_name      TEXT        NOT NULL DEFAULT '',
    channel_type      TEXT        NOT NULL DEFAULT '',
    subscription_id   BIGINT,
    subscription_name TEXT        NOT NULL DEFAULT '',
    event_kind        TEXT        NOT NULL,
    event_status      TEXT        NOT NULL DEFAULT '',
    event_key         TEXT        NOT NULL DEFAULT '',
    severity          TEXT        NOT NULL DEFAULT '',
    env               TEXT        NOT NULL DEFAULT '',
    cluster           TEXT        NOT NULL DEFAULT '',
    title             TEXT        NOT NULL DEFAULT '',
    body              TEXT        NOT NULL DEFAULT '',
    event_json        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status            TEXT        NOT NULL CHECK (status IN ('success','failed','suppressed')),
    reason            TEXT        NOT NULL DEFAULT '',
    duration_ms       INT         NOT NULL DEFAULT 0,
    resend_of         BIGINT,
    triggered_by      TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS obs_notify_records_time    ON obs_notify_records (created_at DESC);
CREATE INDEX IF NOT EXISTS obs_notify_records_dedup   ON obs_notify_records (channel_id, event_key, created_at DESC) WHERE status = 'success';
CREATE INDEX IF NOT EXISTS obs_notify_records_status  ON obs_notify_records (status, created_at DESC);
CREATE INDEX IF NOT EXISTS obs_notify_records_channel ON obs_notify_records (channel_id, created_at DESC);

CREATE TABLE IF NOT EXISTS obs_notify_cursors (
    name       TEXT PRIMARY KEY,
    last_id    BIGINT      NOT NULL DEFAULT 0,
    last_at    TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 兼容：原来每个渠道按自身「级别 / 环境」接收全部告警。为每个已有渠道生成一条等价的告警订阅，升级后行为不变。
INSERT INTO obs_notify_subscriptions (name, description, enabled, event_kinds, severities, envs, channel_ids, created_by)
SELECT '告警 → ' || c.name, '升级时由渠道原接收范围自动生成', TRUE, '{alert}', c.severities, c.envs, ARRAY[c.id], 'system'
FROM obs_channels c
ON CONFLICT (name) DO NOTHING;
