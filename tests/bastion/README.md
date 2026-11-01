# Bastion multi-replica integration

No manifests or acceptance mutations are applied automatically.

## Integration order

1. Back up `/data/recordings` from **both** existing Pods and retain the session
   metadata. An `emptyDir` is lost when its Pod is deleted. Do not roll out yet.
2. Drain all legacy Bastion SSH sessions and stop admission to the old replicas.
   Old binaries neither renew leases nor obey fencing. Mixed old/new Bastion
   processes are unsupported.
3. Let the normal module migrator apply the additive
   `0003_session_ownership.up.sql`. Never rewrite 0001/0002.
4. Verify `nfs-storageclass` uses the expected NFS CSI server
   `192.168.6.3:/storage/storageclass`, then provision the separate
   `deploy/k8s/bastion-storage.yaml` PVC. This requires operator authorization.
5. On the PVC, create a service-owned directory with mode 0750 or stricter.
   Create a regular file `.matrix-bastion-storage-id` containing a unique,
   nonsecret volume identifier. Keep that identifier stable across replicas and
   restores. It must not exceed 256 bytes. The directory must not contain links.
6. Integration owner mounts the **same PVC** at `/data/recordings` in both
   replicas and sets `MATRIX_RECORD_DIR=/data/recordings` and
   `MATRIX_BASTION_RECORDING_STORAGE_ID` to the marker value. Preserve the
   Deployment's other volumes and configuration. The backend independently
   verifies a writable NFS/NFS4 mount, directory permissions and marker.
7. Start the new replicas. SSH admission fails closed while recording storage is
   unavailable; the rest of Matrix can still start. Only SSH is implemented.
8. Run the focused checks and real acceptance below. Verify both replica owners
   before claiming multi-replica acceptance.

Legacy recordings are intentionally unavailable through the new reader until an
operator verifies and imports them with metadata binding the session ID, fence,
volume ID and finalized state. Preserve the original backups; do not bulk mark
legacy files ready. The new recorder never truncates an existing file.

## Lifecycle contract

- Process owner IDs and per-session fencing tokens are random and never reused.
  A live session has a 15-second database lease, renewed every 2 seconds. Database
  failure or rejected renewal cancels its local transports. Stale recovery marks
  an expired session failed with an unknown remote outcome; it never claims a kill
  acknowledgement. Session takeover cannot resurrect an SSH transport.
- Shutdown only cancels the current process's registered sessions. All final
  updates require the original owner/fence and an unexpired active lease.
- Kill is persisted in PostgreSQL and polled by the owner, including during SSH
  setup. HTTP 202 means requested/pending or completed without a kill
  acknowledgement. `killed:true` is returned only after the owner closes its
  transports, joins its I/O workers and finalizes its recording and session.
  Leases bound cancellation; SSH cannot fence a command already running remotely.
- Recordings use `<session-id>-<fence>.cast`; only finalized, DB-bound, regular
  files on the verified shared volume are downloadable. Write/sync/close failures
  fail the session and never advertise a successful recording.
- Command filter actions retain their meaning. `delivery_state=forwarded` proves
  bytes reached the SSH input writer, **not** command execution or exit success.
  Audit insertion failure stops input. A write failure is marked uncertain.
  Already pending commands after a crash retain their uncertain state.

## Checks

From `server`:

```powershell
go test ./internal/modules/bastion -count=1
go vet ./internal/modules/bastion
```

The PostgreSQL tests use `MATRIX_TEST_DATABASE_URL` pointing to a disposable test
database, create a unique schema, apply the real migrations and drop only that
schema. Without a URL those tests explicitly skip, so SQL lifecycle is unverified.

The public API is fixed at `http://192.168.6.3:30080`. Authentication uses an
in-memory `MATRIX_ACCEPTANCE_TOKEN`, or reads the existing admin initial password
through read-only `ssh k8s-master` with batch authentication, strict existing
known-hosts verification and host-key updates disabled. Credentials and tokens
are not printed, passed in argv or written to files. Authenticated fetches reject
redirects. The tracked script contains the implementation; the `.cache` entry
point imports it, so a clean checkout does not depend on ignored implementation.

```powershell
node tests/bastion/live_acceptance.mjs
```

Default execution without a token exits nonzero before network access or
credential lookup. Supply an in-memory `MATRIX_ACCEPTANCE_TOKEN` for read-only
inventory; default mode never reads the cluster Secret or creates filters or
sessions. The existing-password lookup/login is available only with
`MATRIX_BASTION_LIVE_EXEC=1`. Inventory uses a 100-page bound and exits nonzero
because live evidence is still missing. It selects exactly one
authorized SSH asset/account at `192.168.6.4` by default. Set
`MATRIX_BASTION_ASSET_ID` and `MATRIX_BASTION_ACCOUNT_ID` to select an exact
existing grant. An explicit asset may identify another `192.168.6.x` host.
Ambiguous accounts, invalid IDs and unauthorized selections fail closed.

Only `MATRIX_BASTION_LIVE_EXEC=1` enables live mutation: a uniquely named
temporary deny filter and up to four acceptance-owned sessions. Do not enable
this mode until the parent integration has deployed the new application and
completed the storage and legacy-session prerequisites above. A unique marker
must have a `deny`/`blocked` audit entry tied to this exact filter ID; separate
harmless commands must produce both actual output and a forwarded audit entry.
Output markers are split in the command source so PTY echo cannot pass.

The first session stays open while probing for a second active owner; an already
closed probe is never used as a kill target. HTTP 202/pending is accepted as a
request, then the script requires the same request ID, original owner,
`status=killed` and `kill_acknowledged_at` in the final row. Normal close is also
checked. Recording checks require ready/finalized metadata, a complete bounded
asciicast v2 download with valid ordered events, and the actual exercised output.

For cross-Pod proof, the integration owner must establish **two separate
forwards to two specific new Pods in the actual cluster** and set
`MATRIX_BASTION_REPLICA_ORIGINS` to their comma-separated loopback origins, for
example `http://127.0.0.1:18081,http://127.0.0.1:18082`. These must be fixed-Pod
forwards, not Service/NodePort forwards or local mock servers. The public origin
still provides the real inventory; loopback is accepted only for these explicit
transport forwards. The script validates the authenticated actor at each entry,
opens one session on each, and requires distinct process owner IDs. It sends
the second owner's kill request through the first Pod and reads each finalized
recording through the other Pod; the first recording must be byte-identical
through both. Forward-to-Pod binding is the operator's prerequisite, not something
the application API can attest. No forwards are started automatically.

Without the two fixed-Pod origins, public load-balancer probes can exercise
ordinary session behavior but always report a cross-Pod evidence gap and exit
nonzero. Crash/lease-expiry recovery remains a separate controlled acceptance.

Cleanup closes every opened socket, including failed setup, and verifies only
script-owned actor/asset/account/session IDs before any kill. The unique filter
is recovered by exact identity if its creation response is lost; ownership
changes or unresolved sessions preserve it and produce a cleanup gap. Successful
cleanup emits `CLEANUP Bastion owned sessions/filter: complete`; this signal is
independent of acceptance success. Before creation, the child emits its unique
nonsecret filter name, then its acknowledged ID; an interrupted child leaves
these exact reconciliation references in the fixture output. Session/audit rows and finalized recording
files are intentionally retained through the supported API. No existing sessions,
filters, accounts, grants or credentials are modified by this acceptance script.

## Isolated fixture

`.cache/bastion-live-fixture.mjs` prepares a temporary key-only SSH identity when
there are no suitable existing grants. Its default mode performs **no network
access, authentication, key generation or mutation**:

```powershell
node .cache/bastion-live-fixture.mjs
```

Only after the parent deploys the new application:

```powershell
$env:MATRIX_BASTION_FIXTURE_EXEC = "1"
try {
    node .cache/bastion-live-fixture.mjs -execute
} finally {
    Remove-Item Env:MATRIX_BASTION_FIXTURE_EXEC -ErrorAction SilentlyContinue
}
```

Both the environment flag and `-execute` are required. Provide the fixed-Pod
origins above for complete multi-replica acceptance. An existing token is
optional; otherwise the same read-only lookup/login is used.

The fixture creates only a random `mxfx` Linux user on `k8s-node1` via native
Windows `ssh k8s-master` and nested strict SSH. It first verifies the target is
exactly `192.168.6.4:22`, and refuses existing users, groups, homes or mailboxes.
The user has only its new primary group and no usable password (`*`, avoiding
the Linux `!` account lock that can reject public-key login); it is not an
empty-password user. SSH/PAM/AllowUsers policy may still reject this identity,
which remains a live acceptance gap rather than a reason to change host policy.
The one authorized key disables forwarding and user rc while retaining PTY use.

A new RSA private key is generated in process memory only. Only its public key
is sent in the stdin script and placed in this user's authorized_keys. The
private PEM goes through the authenticated account API's existing encryption
path; it is never written as a plaintext key file. The asset host key is obtained
by matching a scan to an already trusted strict SSH fingerprint, using the
current backend's x/crypto v0.36.0 plain-key negotiation order. A mismatch or a
host known only under an unusable algorithm is a gap; TOFU is not used.

The rule must preserve exactly the current admin user ID, this asset, this
account and the requested 15-minute expiry. Empty account scopes, role rules
and expanded grants are rejected. The child receives exact asset/account/user
selection and an allowlist of required environment variables, overriding stale
selection values; unrelated credentials and Node preload options are excluded.
The live SSH identity check additionally requires the expected username,
nonzero UID/GID and no supplemental groups.

Cleanup requires the child's independent session/filter cleanup confirmation
and a paginated terminal-session check before deleting rule, account and asset.
Each delete rechecks exact identity against the initial resource-ID baseline
and refuses unexpected references or cascading deletion. Lost responses use
the unique name, ownership marker, scope and exact expiry for recovery; uncertain
ownership preserves the fixture for reconciliation.

Linux removal checks the exact username, ownership marker, home path, primary
group, public key (when present), symlinks, mounts and running processes, then
uses only `userdel --remove` on that exact user. A partially created owned user
can be removed before authorized_keys exists. No wildcard deletion, forced
process killing or host SSH-policy changes occur. Unexpected state, a residual
group or any failed verification is a nonzero cleanup gap with nonsecret
user/resource references; no unsafe cleanup retry is attempted.
