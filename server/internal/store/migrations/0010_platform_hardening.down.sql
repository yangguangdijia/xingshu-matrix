-- 回滚 v1.29 平台加固：删除一次性连接票据与幂等键表（数据均为短期记录，可安全丢弃）。
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS ws_tickets;
