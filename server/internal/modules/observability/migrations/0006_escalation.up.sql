-- 告警自动升级为故障：规则按告警级别 / 环境 / 标签匹配，命中后声明故障并按排班指派一线值班。
CREATE TABLE IF NOT EXISTS oncall_escalation_rules (
    id                BIGSERIAL PRIMARY KEY,
    name              TEXT        NOT NULL UNIQUE,
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    severities        TEXT[]      NOT NULL DEFAULT '{critical}',
    env               TEXT        NOT NULL DEFAULT '',
    match_labels      JSONB       NOT NULL DEFAULT '{}'::jsonb,
    schedule_id       BIGINT      REFERENCES oncall_schedules(id) ON DELETE SET NULL,
    incident_severity TEXT        NOT NULL DEFAULT 'P2' CHECK (incident_severity IN ('P1','P2','P3','P4')),
    created_by        TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 告警与故障的关联：同一告警在关联故障未解决前不会重复声明故障
CREATE TABLE IF NOT EXISTS oncall_alert_links (
    alert_id    BIGINT      NOT NULL REFERENCES obs_alerts(id) ON DELETE CASCADE,
    incident_id BIGINT      NOT NULL REFERENCES oncall_incidents(id) ON DELETE CASCADE,
    rule_id     BIGINT      REFERENCES oncall_escalation_rules(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (alert_id, incident_id)
);
CREATE INDEX IF NOT EXISTS oncall_alert_links_incident ON oncall_alert_links (incident_id);
