-- Rollback of 0002_agent_sessions: agents fall back to per-replica routing.
DROP TABLE IF EXISTS hosts_relay_keys;
DROP TABLE IF EXISTS hosts_agent_sessions;
