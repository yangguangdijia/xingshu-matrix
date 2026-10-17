-- 星枢 Matrix core schema: identity, RBAC, sessions, audit, approvals, settings, AI.

CREATE TABLE users (
    id                   BIGSERIAL PRIMARY KEY,
    username             TEXT        NOT NULL,
    display_name         TEXT        NOT NULL DEFAULT '',
    email                TEXT        NOT NULL DEFAULT '',
    phone                TEXT        NOT NULL DEFAULT '',
    password_hash        TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    is_builtin           BOOLEAN     NOT NULL DEFAULT FALSE,
    must_change_password BOOLEAN     NOT NULL DEFAULT FALSE,
    failed_attempts      INTEGER     NOT NULL DEFAULT 0,
    locked_until         TIMESTAMPTZ,
    last_login_at        TIMESTAMPTZ,
    last_login_ip        TEXT        NOT NULL DEFAULT '',
    password_changed_at  TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_username_uq ON users (lower(username));

CREATE TABLE roles (
    id          BIGSERIAL PRIMARY KEY,
    code        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    builtin     BOOLEAN     NOT NULL DEFAULT FALSE,
    -- environments this role may operate on; empty array = all environments
    env_scope   TEXT[]      NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE role_permissions (
    role_id    BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    permission TEXT   NOT NULL,
    PRIMARY KEY (role_id, permission)
);

CREATE TABLE user_roles (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id BIGINT NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, role_id)
);
CREATE INDEX user_roles_role_idx ON user_roles (role_id);

-- One row per refresh-token family (a login). Access tokens carry the session id
-- so logout / password change / disable revokes them immediately.
CREATE TABLE sessions (
    id            TEXT PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    refresh_hash  TEXT        NOT NULL,
    user_agent    TEXT        NOT NULL DEFAULT '',
    ip            TEXT        NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL,
    revoked_at    TIMESTAMPTZ
);
CREATE INDEX sessions_user_idx ON sessions (user_id);

-- Per-environment change policy. require_approval=true means write operations
-- against that environment must go through an approval ticket.
CREATE TABLE env_policies (
    env                 TEXT PRIMARY KEY,
    name                TEXT        NOT NULL,
    require_approval    BOOLEAN     NOT NULL DEFAULT FALSE,
    approver_permission TEXT        NOT NULL DEFAULT 'approval:approve',
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO env_policies (env, name, require_approval) VALUES
    ('dev',     '开发', FALSE),
    ('test',    '测试', FALSE),
    ('staging', '预发', FALSE),
    ('prod',    '生产', TRUE);

CREATE TABLE audit_logs (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT,
    username    TEXT        NOT NULL DEFAULT '',
    ip          TEXT        NOT NULL DEFAULT '',
    method      TEXT        NOT NULL,
    path        TEXT        NOT NULL,
    resource    TEXT        NOT NULL DEFAULT '',
    resource_id TEXT        NOT NULL DEFAULT '',
    action      TEXT        NOT NULL DEFAULT '',
    status_code INTEGER     NOT NULL DEFAULT 0,
    result      TEXT        NOT NULL CHECK (result IN ('success','failure')),
    duration_ms INTEGER     NOT NULL DEFAULT 0,
    request_id  TEXT        NOT NULL DEFAULT '',
    user_agent  TEXT        NOT NULL DEFAULT '',
    detail      JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_logs_created_idx  ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_user_idx     ON audit_logs (user_id, created_at DESC);
CREATE INDEX audit_logs_resource_idx ON audit_logs (resource, created_at DESC);

CREATE TABLE approval_tickets (
    id             BIGSERIAL PRIMARY KEY,
    type           TEXT        NOT NULL,
    title          TEXT        NOT NULL,
    env            TEXT        NOT NULL DEFAULT '',
    reason         TEXT        NOT NULL DEFAULT '',
    payload        JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status         TEXT        NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','approved','rejected','cancelled','executed','failed')),
    applicant_id   BIGINT      NOT NULL,
    applicant_name TEXT        NOT NULL DEFAULT '',
    approver_id    BIGINT,
    approver_name  TEXT        NOT NULL DEFAULT '',
    comment        TEXT        NOT NULL DEFAULT '',
    result         JSONB,
    error          TEXT        NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at     TIMESTAMPTZ,
    executed_at    TIMESTAMPTZ
);
CREATE INDEX approval_tickets_status_idx    ON approval_tickets (status, created_at DESC);
CREATE INDEX approval_tickets_applicant_idx ON approval_tickets (applicant_id, created_at DESC);

CREATE TABLE settings (
    key         TEXT PRIMARY KEY,
    value       JSONB       NOT NULL,
    secret      BOOLEAN     NOT NULL DEFAULT FALSE,
    description TEXT        NOT NULL DEFAULT '',
    updated_by  BIGINT,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE ai_models (
    id            BIGSERIAL PRIMARY KEY,
    name          TEXT        NOT NULL UNIQUE,
    provider      TEXT        NOT NULL DEFAULT 'openai-compatible',
    base_url      TEXT        NOT NULL,
    api_key_enc   TEXT        NOT NULL DEFAULT '',
    model         TEXT        NOT NULL,
    temperature   REAL        NOT NULL DEFAULT 0.2,
    max_tokens    INTEGER     NOT NULL DEFAULT 0,
    extra_headers JSONB       NOT NULL DEFAULT '{}'::jsonb,
    is_default    BOOLEAN     NOT NULL DEFAULT FALSE,
    enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ai_models_one_default ON ai_models (is_default) WHERE is_default;

CREATE TABLE ai_prompt_templates (
    id          BIGSERIAL PRIMARY KEY,
    scene       TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    content     TEXT        NOT NULL,
    is_default  BOOLEAN     NOT NULL DEFAULT FALSE,
    enabled     BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scene, name)
);
CREATE UNIQUE INDEX ai_prompt_templates_scene_default ON ai_prompt_templates (scene) WHERE is_default;

-- Tool calls proposed by the model that need explicit user confirmation.
CREATE TABLE ai_tool_calls (
    id          TEXT PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    tool        TEXT        NOT NULL,
    tool_call_id TEXT       NOT NULL DEFAULT '',
    arguments   JSONB       NOT NULL DEFAULT '{}'::jsonb,
    status      TEXT        NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','executed','rejected','failed')),
    result      JSONB,
    error       TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at  TIMESTAMPTZ
);
CREATE INDEX ai_tool_calls_user_idx ON ai_tool_calls (user_id, created_at DESC);

-- Built-in roles. Permissions use resource:action; '*' matches everything,
-- 'resource:*' every action on a resource and '*:read' read on every resource.
INSERT INTO roles (code, name, description, builtin) VALUES
    ('admin',    '超级管理员', '拥有全部权限',                   TRUE),
    ('operator', '运维工程师', '读取全部资源，执行非用户/角色管理类操作', TRUE),
    ('viewer',   '只读用户',   '只读访问全部资源',               TRUE),
    ('approver', '审批人',     '审批生产变更',                   TRUE);

INSERT INTO role_permissions (role_id, permission)
SELECT id, '*' FROM roles WHERE code = 'admin';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p FROM roles r, unnest(ARRAY['*:read','*:write','*:exec','ai:chat','approval:create']) AS p
WHERE r.code = 'operator';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p FROM roles r, unnest(ARRAY['*:read','ai:chat']) AS p
WHERE r.code = 'viewer';

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p FROM roles r, unnest(ARRAY['*:read','approval:approve']) AS p
WHERE r.code = 'approver';
