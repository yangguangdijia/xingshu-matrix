-- v1.29 企业空间与数据范围（KubeSphere 式：企业空间 → 项目 → 成员）。
-- 向后兼容：未创建企业空间、也没有范围化角色绑定时，任何用户的权限与可见范围都与旧版本一致。
-- user_roles 仍是「全局绑定」；role_bindings 只承载新增的范围化绑定（也可为用户组做全局 / 环境绑定）。

CREATE TABLE IF NOT EXISTS workspaces (
    id                 BIGSERIAL PRIMARY KEY,
    name               TEXT        NOT NULL,
    description        TEXT        NOT NULL DEFAULT '',
    -- 空间管理员（展示用的主要负责人；管理权限以 workspace_members.role 为准）
    manager_id         BIGINT      REFERENCES users(id) ON DELETE SET NULL,
    -- 空间总配额上限，0 表示不限制。CPU 以毫核计，内存以字节计。
    cpu_limit_milli    BIGINT      NOT NULL DEFAULT 0 CHECK (cpu_limit_milli >= 0),
    memory_limit_bytes BIGINT      NOT NULL DEFAULT 0 CHECK (memory_limit_bytes >= 0),
    created_by         TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_name_uq ON workspaces (lower(name));

-- 空间成员：用户或用户组，二选一。
CREATE TABLE IF NOT EXISTS workspace_members (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id BIGINT      NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id      BIGINT      REFERENCES users(id) ON DELETE CASCADE,
    group_id     BIGINT      REFERENCES user_groups(id) ON DELETE CASCADE,
    role         TEXT        NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(user_id, group_id) = 1)
);
CREATE UNIQUE INDEX IF NOT EXISTS workspace_members_user_uq ON workspace_members (workspace_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS workspace_members_group_uq ON workspace_members (workspace_id, group_id) WHERE group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS workspace_members_user_idx ON workspace_members (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS workspace_members_group_idx ON workspace_members (group_id) WHERE group_id IS NOT NULL;

-- 空间项目：集群 ID + 命名空间；一个命名空间只属于一个空间。
-- cluster_clusters 属于集群模块（迁移晚于核心），因此不建外键；集群删除后残留行在列表里标记为「集群不存在」。
CREATE TABLE IF NOT EXISTS workspace_projects (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id BIGINT      NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    cluster_id   BIGINT      NOT NULL CHECK (cluster_id > 0),
    namespace    TEXT        NOT NULL CHECK (namespace ~ '^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'),
    created_by   TEXT        NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, namespace)
);
CREATE INDEX IF NOT EXISTS workspace_projects_ws_idx ON workspace_projects (workspace_id);

-- 堡垒机资产节点（资产组）绑定空间：本轮只建数据模型，堡垒机列表尚未按空间过滤。
CREATE TABLE IF NOT EXISTS workspace_asset_nodes (
    workspace_id BIGINT      NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    node_id      BIGINT      NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, node_id),
    UNIQUE (node_id)
);

-- 范围化角色绑定。scope_value 约定：
--   global → ''；env → 环境编码；cluster → 集群 ID；workspace → 空间 ID；project → '<集群ID>/<命名空间>'
CREATE TABLE IF NOT EXISTS role_bindings (
    id          BIGSERIAL PRIMARY KEY,
    role_id     BIGINT      NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    user_id     BIGINT      REFERENCES users(id) ON DELETE CASCADE,
    group_id    BIGINT      REFERENCES user_groups(id) ON DELETE CASCADE,
    scope       TEXT        NOT NULL DEFAULT 'global' CHECK (scope IN ('global','env','cluster','workspace','project')),
    scope_value TEXT        NOT NULL DEFAULT '',
    created_by  TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(user_id, group_id) = 1),
    CHECK ((scope = 'global') = (scope_value = ''))
);
CREATE UNIQUE INDEX IF NOT EXISTS role_bindings_uq
    ON role_bindings (role_id, COALESCE(user_id, 0), COALESCE(group_id, 0), scope, scope_value);
CREATE INDEX IF NOT EXISTS role_bindings_user_idx ON role_bindings (user_id) WHERE user_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS role_bindings_group_idx ON role_bindings (group_id) WHERE group_id IS NOT NULL;
