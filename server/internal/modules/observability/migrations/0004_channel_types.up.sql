-- Observability migration 4: notification channels also support Slack
-- incoming webhooks and generic (HMAC-signed) webhooks.
ALTER TABLE obs_channels DROP CONSTRAINT IF EXISTS obs_channels_type_check;
ALTER TABLE obs_channels ADD CONSTRAINT obs_channels_type_check
    CHECK (type IN ('dingtalk','feishu','wecom','email','slack','webhook'));
