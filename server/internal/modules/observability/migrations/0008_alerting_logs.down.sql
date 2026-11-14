-- 撤销 0008_alerting_logs.up.sql（逆序）。

DROP TABLE IF EXISTS obs_log_collectors;

DROP INDEX IF EXISTS obs_cert_notices_sent;
DROP TABLE IF EXISTS obs_cert_notices;

-- 恢复 0007 的 event_kind 约束；先清掉 certificate 模板，否则约束无法加回。
DELETE FROM obs_notify_templates WHERE event_kind = 'certificate';
ALTER TABLE obs_notify_templates DROP CONSTRAINT IF EXISTS obs_notify_templates_event_kind_check;
ALTER TABLE obs_notify_templates ADD CONSTRAINT obs_notify_templates_event_kind_check
    CHECK (event_kind IN ('alert','incident','pipeline','approval','test'));

ALTER TABLE oncall_escalation_rules DROP CONSTRAINT IF EXISTS oncall_escalation_rules_for_minutes_check;
ALTER TABLE oncall_escalation_rules DROP COLUMN IF EXISTS for_minutes;

DROP INDEX IF EXISTS obs_local_silences_active;
DROP TABLE IF EXISTS obs_local_silences;
