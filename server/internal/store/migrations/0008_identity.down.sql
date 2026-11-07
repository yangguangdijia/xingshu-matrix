-- 回滚 v1.29 身份与令牌（0008_identity.up.sql）。
DROP TABLE IF EXISTS mfa_challenges;
DROP TABLE IF EXISTS user_mfa;
DROP TABLE IF EXISTS api_tokens;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_account_type_chk;
ALTER TABLE users DROP COLUMN IF EXISTS description;
ALTER TABLE users DROP COLUMN IF EXISTS owner_id;
ALTER TABLE users DROP COLUMN IF EXISTS account_type;
