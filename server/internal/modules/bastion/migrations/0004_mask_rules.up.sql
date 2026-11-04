-- 终端输出脱敏规则：SSH 会话输出在发送到浏览器、写入录像之前按正则替换。
CREATE TABLE IF NOT EXISTS bastion_mask_rules (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  pattern     TEXT NOT NULL,
  replacement TEXT NOT NULL DEFAULT '******',
  enabled     BOOLEAN NOT NULL DEFAULT TRUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
