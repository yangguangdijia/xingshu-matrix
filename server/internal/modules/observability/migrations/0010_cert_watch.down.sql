-- 撤销 0010_cert_watch.up.sql（逆序）。obs_cert_notices 属于 0008，不在此删除。
DROP TABLE IF EXISTS obs_cert_watch_config;
DROP INDEX IF EXISTS obs_cert_status_not_after;
DROP TABLE IF EXISTS obs_cert_status;
DROP TABLE IF EXISTS obs_cert_domains;
