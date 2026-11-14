-- 登录 ACL 新增 review 动作（需审批后登录，对应 JumpServer「登录复核」）与审批免签窗口：
-- 审批通过后在 exempt_minutes 分钟内，同一用户 + 资产 + 账号再次连接不再发起审批。
ALTER TABLE bastion_login_acls ADD COLUMN IF NOT EXISTS exempt_minutes INT NOT NULL DEFAULT 0;
ALTER TABLE bastion_login_acls DROP CONSTRAINT IF EXISTS bastion_login_acls_exempt_minutes_check;
ALTER TABLE bastion_login_acls ADD CONSTRAINT bastion_login_acls_exempt_minutes_check CHECK (exempt_minutes BETWEEN 0 AND 10080);
ALTER TABLE bastion_login_acls DROP CONSTRAINT IF EXISTS bastion_login_acls_action_check;
ALTER TABLE bastion_login_acls ADD CONSTRAINT bastion_login_acls_action_check CHECK (action IN ('allow','deny','review'));

CREATE TABLE IF NOT EXISTS bastion_acl_exemptions (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT      NOT NULL,
    account     TEXT        NOT NULL DEFAULT '',
    asset_id    BIGINT      NOT NULL,
    acl_id      BIGINT      REFERENCES bastion_login_acls(id) ON DELETE SET NULL,
    approval_id BIGINT,
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    revoked_by  BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS bastion_acl_exemptions_active ON bastion_acl_exemptions (user_id, asset_id, account) WHERE revoked_at IS NULL;
