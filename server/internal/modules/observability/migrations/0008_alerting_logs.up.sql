-- v1.29 告警与日志：
--   1. 平台本地静默规则：对平台自己发出的通知生效（未接入 Alertmanager 时也可用），生产环境可配置为先审批；
--   2. 告警自动转故障：升级规则增加「持续时间」（告警持续触发满 N 分钟才建故障）；
--   3. 证书到期提醒：通知中心新增 certificate 事件类型，按 30/7/1 天阈值去重发送；
--   4. 日志采集一键部署：记录各集群日志采集器与自动绑定的日志数据源。

CREATE TABLE IF NOT EXISTS obs_local_silences (
    id          BIGSERIAL PRIMARY KEY,
    env         TEXT        NOT NULL,
    cluster     TEXT        NOT NULL DEFAULT '',
    matchers    JSONB       NOT NULL DEFAULT '[]'::jsonb,
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    comment     TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    state       TEXT        NOT NULL DEFAULT 'active' CHECK (state IN ('pending','active','ended','rejected')),
    approval_id BIGINT,
    ended_by    TEXT        NOT NULL DEFAULT '',
    ended_at    TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at > starts_at)
);
CREATE INDEX IF NOT EXISTS obs_local_silences_active ON obs_local_silences (state, ends_at);

ALTER TABLE oncall_escalation_rules ADD COLUMN IF NOT EXISTS for_minutes INT NOT NULL DEFAULT 0;
ALTER TABLE oncall_escalation_rules DROP CONSTRAINT IF EXISTS oncall_escalation_rules_for_minutes_check;
ALTER TABLE oncall_escalation_rules ADD CONSTRAINT oncall_escalation_rules_for_minutes_check CHECK (for_minutes BETWEEN 0 AND 1440);

ALTER TABLE obs_notify_templates DROP CONSTRAINT IF EXISTS obs_notify_templates_event_kind_check;
ALTER TABLE obs_notify_templates ADD CONSTRAINT obs_notify_templates_event_kind_check
    CHECK (event_kind IN ('alert','incident','pipeline','approval','certificate','test'));

-- 证书提醒去重：同一证书（含到期时间，续期后重新计数）同一阈值只发一次。
CREATE TABLE IF NOT EXISTS obs_cert_notices (
    cluster_id BIGINT      NOT NULL,
    source     TEXT        NOT NULL,
    namespace  TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    not_after  TIMESTAMPTZ NOT NULL,
    threshold  INT         NOT NULL,
    sent_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, source, namespace, name, not_after, threshold)
);
CREATE INDEX IF NOT EXISTS obs_cert_notices_sent ON obs_cert_notices (sent_at);

-- 日志采集器：由组件中心安装的 Fluent Bit（DaemonSet 带平台标签），发现后自动创建/绑定日志数据源。
CREATE TABLE IF NOT EXISTS obs_log_collectors (
    cluster_id    BIGINT      NOT NULL,
    namespace     TEXT        NOT NULL,
    name          TEXT        NOT NULL,
    collector     TEXT        NOT NULL DEFAULT 'fluent-bit',
    output_type   TEXT        NOT NULL CHECK (output_type IN ('loki','elasticsearch')),
    output_url    TEXT        NOT NULL,
    cluster_label TEXT        NOT NULL DEFAULT '',
    datasource_id BIGINT,
    status        TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','bound','conflict','failed')),
    message       TEXT        NOT NULL DEFAULT '',
    synced_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, namespace, name)
);
