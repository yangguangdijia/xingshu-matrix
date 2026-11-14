-- 撤销 0009_alert_claim.up.sql（逆序）。
DROP INDEX IF EXISTS oncall_alert_links_alert;
ALTER TABLE obs_alerts DROP COLUMN IF EXISTS claim_silence_until;
ALTER TABLE obs_alerts DROP COLUMN IF EXISTS claim_silence_id;
ALTER TABLE obs_alerts DROP COLUMN IF EXISTS claim_comment;
