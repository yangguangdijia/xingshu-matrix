-- 按 JumpServer 信息架构补全：资产节点、平台、授权动作 / 节点 / 用户组、命令过滤规则组与允许动作、
-- 登录 ACL、资产登录日志、账号连通性验证、数据库控制台审计。全部为增量变更。

-- 资产节点（资产树）。删除节点前由接口检查子节点与资产，外键只做兜底。
CREATE TABLE IF NOT EXISTS bastion_nodes (
    id         BIGSERIAL PRIMARY KEY,
    parent_id  BIGINT REFERENCES bastion_nodes(id) ON DELETE RESTRICT,
    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS bastion_nodes_name_uq ON bastion_nodes (COALESCE(parent_id, 0), name);

-- 平台：资产的系统 / 数据库类型与默认协议端口。内置平台不可删除。
CREATE TABLE IF NOT EXISTS bastion_platforms (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    category    TEXT NOT NULL CHECK (category IN ('host','database')),
    type        TEXT NOT NULL CHECK (type IN ('linux','unix','windows','mysql','postgresql')),
    protocol    TEXT NOT NULL CHECK (protocol IN ('ssh','rdp','mysql','postgres')),
    port        INT  NOT NULL CHECK (port BETWEEN 1 AND 65535),
    charset     TEXT NOT NULL DEFAULT 'utf-8',
    description TEXT NOT NULL DEFAULT '',
    builtin     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO bastion_platforms (name,category,type,protocol,port,description,builtin) VALUES
    ('Linux','host','linux','ssh',22,'通用 Linux 服务器，SSH 连接',TRUE),
    ('Unix','host','unix','ssh',22,'AIX / Solaris / BSD 等，SSH 连接',TRUE),
    ('Windows','host','windows','rdp',3389,'Windows 服务器（远程桌面连接器未接入，仅登记）',TRUE),
    ('MySQL','database','mysql','mysql',3306,'MySQL / MariaDB，数据库控制台',TRUE),
    ('PostgreSQL','database','postgresql','postgres',5432,'PostgreSQL，数据库控制台',TRUE)
ON CONFLICT (name) DO NOTHING;

ALTER TABLE bastion_assets ADD COLUMN IF NOT EXISTS node_id BIGINT REFERENCES bastion_nodes(id) ON DELETE SET NULL;
ALTER TABLE bastion_assets ADD COLUMN IF NOT EXISTS platform_id BIGINT REFERENCES bastion_platforms(id) ON DELETE SET NULL;
UPDATE bastion_assets SET platform_id=(SELECT id FROM bastion_platforms WHERE name='Linux') WHERE platform_id IS NULL AND protocol='ssh';

-- 账号连通性验证结果
ALTER TABLE bastion_accounts ADD COLUMN IF NOT EXISTS verify_status TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE bastion_accounts ADD COLUMN IF NOT EXISTS verify_message TEXT NOT NULL DEFAULT '';
ALTER TABLE bastion_accounts ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;

-- 资产授权：主体可为用户组；可授权节点（含子节点）；动作；生效时间
ALTER TABLE bastion_rules DROP CONSTRAINT IF EXISTS bastion_rules_subject_type_check;
ALTER TABLE bastion_rules ADD CONSTRAINT bastion_rules_subject_type_check CHECK (subject_type IN ('user','role','group'));
ALTER TABLE bastion_rules ADD COLUMN IF NOT EXISTS node_ids BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE bastion_rules ADD COLUMN IF NOT EXISTS actions TEXT[] NOT NULL DEFAULT '{connect,upload,download,copy,paste}';
ALTER TABLE bastion_rules ADD COLUMN IF NOT EXISTS starts_at TIMESTAMPTZ;

-- 命令过滤：规则组、作用资产（空 = 全部资产）、动作新增 allow（白名单，命中后放行且不再匹配后续规则）
ALTER TABLE bastion_filters ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT '默认规则组';
ALTER TABLE bastion_filters ADD COLUMN IF NOT EXISTS asset_ids BIGINT[] NOT NULL DEFAULT '{}';
ALTER TABLE bastion_filters DROP CONSTRAINT IF EXISTS bastion_filters_action_check;
ALTER TABLE bastion_filters ADD CONSTRAINT bastion_filters_action_check CHECK (action IN ('deny','confirm','approve','allow'));

-- 登录 ACL：按用户 / 角色 / 用户组、资产、来源 IP、星期与时段控制连接；按优先级取第一条命中的规则
CREATE TABLE IF NOT EXISTS bastion_login_acls (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    priority     INT  NOT NULL DEFAULT 100 CHECK (priority BETWEEN 1 AND 100000),
    subject_type TEXT NOT NULL DEFAULT 'all' CHECK (subject_type IN ('all','user','role','group')),
    subject      TEXT NOT NULL DEFAULT '',
    asset_ids    BIGINT[] NOT NULL DEFAULT '{}',
    ip_ranges    TEXT[]   NOT NULL DEFAULT '{}',
    weekdays     INT[]    NOT NULL DEFAULT '{}',
    time_from    TEXT     NOT NULL DEFAULT '',
    time_to      TEXT     NOT NULL DEFAULT '',
    action       TEXT     NOT NULL CHECK (action IN ('allow','deny')),
    enabled      BOOLEAN  NOT NULL DEFAULT TRUE,
    description  TEXT     NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 资产登录日志：每次经堡垒机连接资产的尝试（成功 / 拒绝 / 失败）
CREATE TABLE IF NOT EXISTS bastion_login_logs (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL DEFAULT 0,
    username    TEXT NOT NULL DEFAULT '',
    asset_id    BIGINT NOT NULL DEFAULT 0,
    asset_name  TEXT NOT NULL DEFAULT '',
    account     TEXT NOT NULL DEFAULT '',
    protocol    TEXT NOT NULL DEFAULT '',
    client_ip   TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL CHECK (result IN ('success','denied','failed')),
    reason      TEXT NOT NULL DEFAULT '',
    session_id  BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS bastion_login_logs_created_idx ON bastion_login_logs (created_at DESC);

-- 数据库控制台：每条 SQL 的执行 / 工单 / 拒绝记录。SQL 原文加密保存，列表只返回摘要。
CREATE TABLE IF NOT EXISTS bastion_db_queries (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL,
    username      TEXT NOT NULL,
    asset_id      BIGINT NOT NULL,
    asset_name    TEXT NOT NULL,
    account_id    BIGINT NOT NULL,
    account_user  TEXT NOT NULL,
    database_name TEXT NOT NULL DEFAULT '',
    level         TEXT NOT NULL CHECK (level IN ('read','write','high','forbidden')),
    reason        TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL DEFAULT '',
    sql_enc       TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL CHECK (status IN ('executed','failed','rejected','pending','denied')),
    error         TEXT NOT NULL DEFAULT '',
    row_count     INT NOT NULL DEFAULT 0,
    rows_affected BIGINT NOT NULL DEFAULT 0,
    duration_ms   BIGINT NOT NULL DEFAULT 0,
    ticket_id     BIGINT,
    client_ip     TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS bastion_db_queries_created_idx ON bastion_db_queries (created_at DESC);
CREATE INDEX IF NOT EXISTS bastion_db_queries_ticket_idx ON bastion_db_queries (ticket_id) WHERE ticket_id IS NOT NULL;
