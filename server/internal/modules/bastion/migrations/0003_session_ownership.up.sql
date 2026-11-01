-- Additive migration. Drain old binaries before enabling the lease-aware replicas.
ALTER TABLE bastion_sessions
    ADD COLUMN owner_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN fence_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN heartbeat_at TIMESTAMPTZ,
    ADD COLUMN lease_expires_at TIMESTAMPTZ,
    ADD COLUMN kill_request_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN kill_requested_at TIMESTAMPTZ,
    ADD COLUMN kill_requested_by BIGINT,
    ADD COLUMN kill_acknowledged_at TIMESTAMPTZ,
    ADD COLUMN recording_state TEXT NOT NULL DEFAULT 'unavailable',
    ADD COLUMN recording_storage_id TEXT NOT NULL DEFAULT '';

CREATE INDEX bastion_sessions_lease_idx
    ON bastion_sessions (lease_expires_at) WHERE status='active';
CREATE UNIQUE INDEX bastion_sessions_fence_idx
    ON bastion_sessions (fence_token) WHERE fence_token<>'';

-- A command is a submission, never proof that a shell command executed successfully.
ALTER TABLE bastion_commands
    ADD COLUMN delivery_state TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN delivery_error TEXT NOT NULL DEFAULT '',
    ADD COLUMN forwarded_at TIMESTAMPTZ;
