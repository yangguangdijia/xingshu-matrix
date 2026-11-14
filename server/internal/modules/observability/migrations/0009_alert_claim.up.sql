-- v1.30 告警认领与时间线：
--   认领人/认领时间沿用 obs_alerts.acked_by / acked_at（接口同时以 claimed_by / claimed_at 返回）；
--   新增认领备注与认领时在 Alertmanager 创建的静默（id 与到期时间），供时间线与关闭时清理使用。
ALTER TABLE obs_alerts ADD COLUMN IF NOT EXISTS claim_comment       TEXT        NOT NULL DEFAULT '';
ALTER TABLE obs_alerts ADD COLUMN IF NOT EXISTS claim_silence_id    TEXT        NOT NULL DEFAULT '';
ALTER TABLE obs_alerts ADD COLUMN IF NOT EXISTS claim_silence_until TIMESTAMPTZ;

-- 时间线按告警读取关联故障。
CREATE INDEX IF NOT EXISTS oncall_alert_links_alert ON oncall_alert_links (alert_id, created_at);
