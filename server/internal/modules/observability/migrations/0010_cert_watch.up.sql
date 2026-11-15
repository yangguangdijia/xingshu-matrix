-- v1.29 证书到期提醒（g-cert）：
--   1. obs_cert_domains：用户配置的域名 TLS 探测目标；
--   2. obs_cert_status：最近一次扫描得到的证书快照（集群 kubernetes.io/tls Secret 与域名探测）；
--   3. obs_cert_watch_config：扫描周期、提醒阈值、开关与最近一次扫描结果（单行）。
-- 提醒去重沿用 0008 的 obs_cert_notices（域名来源 cluster_id=0、namespace=''、name=host:port）。

CREATE TABLE IF NOT EXISTS obs_cert_domains (
    id         BIGSERIAL PRIMARY KEY,
    host       TEXT        NOT NULL,
    port       INT         NOT NULL DEFAULT 443 CHECK (port BETWEEN 1 AND 65535),
    env        TEXT        NOT NULL DEFAULT '',
    remark     TEXT        NOT NULL DEFAULT '',
    created_by TEXT        NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (host, port)
);

CREATE TABLE IF NOT EXISTS obs_cert_status (
    source       TEXT        NOT NULL CHECK (source IN ('secret','domain')),
    cluster_id   BIGINT      NOT NULL DEFAULT 0,
    namespace    TEXT        NOT NULL DEFAULT '',
    name         TEXT        NOT NULL,
    cluster_name TEXT        NOT NULL DEFAULT '',
    env          TEXT        NOT NULL DEFAULT '',
    subject      TEXT        NOT NULL DEFAULT '',
    issuer       TEXT        NOT NULL DEFAULT '',
    dns_names    TEXT        NOT NULL DEFAULT '',
    not_after    TIMESTAMPTZ,
    error        TEXT        NOT NULL DEFAULT '',
    scanned_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, cluster_id, namespace, name)
);
CREATE INDEX IF NOT EXISTS obs_cert_status_not_after ON obs_cert_status (not_after);

CREATE TABLE IF NOT EXISTS obs_cert_watch_config (
    id             INT         PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    enabled        BOOLEAN     NOT NULL DEFAULT true,
    interval_hours INT         NOT NULL DEFAULT 6 CHECK (interval_hours BETWEEN 1 AND 168),
    thresholds     INT[]       NOT NULL DEFAULT '{30,14,7,1}',
    last_scan_at   TIMESTAMPTZ,
    last_result    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_by     TEXT        NOT NULL DEFAULT '',
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO obs_cert_watch_config (id) VALUES (1) ON CONFLICT (id) DO NOTHING;
