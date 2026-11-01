import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const acceptanceContract = Object.freeze({
  version: 2,
  environment: ["MATRIX_BASTION_LIVE_EXEC", "MATRIX_BASTION_ASSET_ID", "MATRIX_BASTION_ACCOUNT_ID", "MATRIX_ACCEPTANCE_TOKEN"],
});

export async function runLiveAcceptance() {
  const origin = "http://192.168.6.3:30080";
  const targetHost = "192.168.6.4";
  const liveExec = process.env.MATRIX_BASTION_LIVE_EXEC === "1";
  const gaps = [];
  const owned = [];
  const sleep = (ms) => new Promise((done) => setTimeout(done, ms));
  let token = process.env.MATRIX_ACCEPTANCE_TOKEN || "";
  let actor;
  let filter;
  let filterAttempted = false;
  let filterConfirmed = false;
  const marker = `matrix_bastion_accept_${randomBytes(12).toString("hex")}`;

  // Never include child-process messages, response bodies or WebSocket URLs in errors.
  class AcceptanceFailure extends Error {}
  const fail = (message) => { throw new AcceptanceFailure(message); };
  const idOf = (value) => {
    const id = Number(value);
    return Number.isSafeInteger(id) && id > 0 ? id : 0;
  };
  function rows(value) {
    if (Array.isArray(value)) return value;
    if (Array.isArray(value?.items)) return value.items;
    return fail("unexpected inventory response; refusing incomplete selection or cleanup");
  }
  async function request(path, options = {}, endpoint = origin) {
    let response;
    try {
      response = await fetch(`${endpoint}/api/v1${path}`, {
        ...options, redirect: "error", signal: AbortSignal.timeout(15000),
        headers: {
          ...(options.body ? { "content-type": "application/json" } : {}),
          ...(token ? { authorization: `Bearer ${token}` } : {}),
        },
      });
    } catch { return fail(`${options.method || "GET"} ${path}: transport failure`); }
    let body;
    try { body = await response.json(); }
    catch { return fail(`${options.method || "GET"} ${path}: HTTP ${response.status}, invalid JSON`); }
    if (!response.ok || body?.code !== 0) return fail(`${options.method || "GET"} ${path}: HTTP ${response.status}, API request rejected`);
    return body.data;
  }
  async function all(path) {
    const result = [];
    for (let page = 1; page <= 100; page += 1) {
      const value = await request(`${path}${path.includes("?") ? "&" : "?"}page=${page}&page_size=200`);
      const batch = rows(value);
      result.push(...batch);
      if (Array.isArray(value)) return result;
      if (!Number.isSafeInteger(value?.total) || value.total < result.length) return fail("invalid inventory pagination");
      if (result.length === value.total) return result;
      if (!batch.length) return fail("incomplete inventory pagination");
    }
    return fail("inventory exceeds bounded pagination");
  }
  async function authenticate() {
    if (!token) {
      let password = "";
      try {
        password = execFileSync("ssh", [
          "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
          "-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no",
          "k8s-master",
          'kubectl -n xingshu-system get secret matrix-secrets -o jsonpath="{.data.MATRIX_ADMIN_INITIAL_PASSWORD}" | base64 -d',
        ], { encoding: "utf8", stdio: ["ignore", "pipe", "ignore"], timeout: 15000 }).trim();
        if (!password) fail("admin credential unavailable");
        const login = await request("/auth/login", {
          method: "POST", body: JSON.stringify({ username: "admin", password }),
        });
        token = login?.token?.access_token || "";
        if (!token) fail("login did not return an access token");
      } catch { fail("credential lookup/login failed"); }
      finally { password = ""; }
    }
    actor = await request("/auth/me");
    if (!idOf(actor?.user_id)) fail("authenticated actor has no valid user ID");
  }
  function replicaOrigins() {
    const setting = process.env.MATRIX_BASTION_REPLICA_ORIGINS;
    if (!setting) return [];
    const entries = setting.split(",");
    if (entries.length !== 2) fail("replica origins must contain exactly two loopback HTTP origins");
    const result = entries.map((entry) => {
      let url;
      try { url = new URL(entry); } catch { return fail("invalid replica origin"); }
      if (url.protocol !== "http:" || url.hostname !== "127.0.0.1" || !url.port ||
          url.username || url.password || url.pathname !== "/" || url.search || url.hash) {
        fail("replica origins must be explicit http://127.0.0.1:port origins");
      }
      return url.origin;
    });
    if (result[0] === result[1]) fail("replica origins must be distinct fixed-Pod forwards");
    return result;
  }
  async function detail(session, endpoint = origin) {
    const value = await request(`/bastion/sessions/${session.id}`, {}, endpoint);
    const row = value?.session;
    if (idOf(row?.id) !== session.id || Number(row.user_id) !== Number(actor.user_id) ||
        Number(row.asset_id) !== session.assetId || Number(row.account_id) !== session.accountId ||
        (session.ownerId && row.owner_id !== session.ownerId)) {
      fail("session identity differs from the script-owned asset/account/actor/owner");
    }
    return value;
  }
  async function waitFor(session, predicate, timeout = 20000, endpoint = origin) {
    const deadline = Date.now() + timeout;
    while (Date.now() < deadline) {
      const value = await detail(session, endpoint);
      if (predicate(value.session)) return value.session;
      await sleep(250);
    }
    return fail(`owned session ${session.id} did not reach expected state`);
  }
  async function openSession(grant, endpoint = origin) {
    const url = new URL(`${endpoint}/api/v1/bastion/sessions/ssh/ws`);
    url.protocol = "ws:";
    url.searchParams.set("asset", String(grant.asset_id));
    url.searchParams.set("account", String(grant.account_id));
    url.searchParams.set("access_token", token);
    const ws = new WebSocket(url, "xterm");
    const session = {
      ws, id: 0, ownerId: "", output: "", failure: false, endpoint,
      assetId: Number(grant.asset_id), accountId: Number(grant.account_id),
    };
    // Track before waiting so even setup failures close their socket in finally.
    owned.push(session);
    await new Promise((done, reject) => {
      let settled = false;
      const finish = (error) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        if (error) reject(error); else done();
      };
      const timer = setTimeout(() => {
        session.failure = true;
        try { ws.close(); } catch { /* finally retries close and audits owned IDs */ }
        finish(new AcceptanceFailure("SSH session setup timed out"));
      }, 25000);
      ws.onmessage = (event) => {
        let message;
        try { message = JSON.parse(event.data); } catch { session.failure = true; return; }
        if (message.type === "session") {
          const id = idOf(message.data);
          if (!id || (session.id && session.id !== id)) {
            session.failure = true;
            finish(new AcceptanceFailure("invalid session identity"));
          } else {
            session.id = id;
            finish();
          }
        } else if (message.type === "output") {
          session.output = (session.output + String(message.data || "")).slice(-512000);
        } else if (message.type === "error") {
          session.failure = true;
          finish(new AcceptanceFailure("SSH session setup failed"));
        }
      };
      ws.onerror = () => { session.failure = true; finish(new AcceptanceFailure("SSH WebSocket failed")); };
      ws.onclose = () => { finish(new AcceptanceFailure("SSH WebSocket closed before session creation")); };
    });
    const row = await waitFor(session, (item) => item.status !== "active" || Boolean(item.owner_id));
    if (row.status !== "active") fail("SSH session failed before owner verification");
    session.ownerId = row.owner_id;
    return session;
  }
  async function outputCommand(session, command, expected) {
    if (session.failure || session.ws.readyState !== WebSocket.OPEN) fail("owned SSH session is not usable");
    session.ws.send(JSON.stringify({ type: "input", data: `${command}\r` }));
    const deadline = Date.now() + 15000;
    while (Date.now() < deadline && !session.output.replace(/\r/g, "").includes(`\n${expected}\n`)) {
      if (session.failure || session.ws.readyState !== WebSocket.OPEN) fail("SSH session failed before command output");
      await sleep(100);
    }
    if (!session.output.replace(/\r/g, "").includes(`\n${expected}\n`)) fail("command output not observed separately from PTY echo");
  }
  async function audited(session, markerText, predicate) {
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) {
      const value = await detail(session);
      if (rows(value.commands).some((entry) => entry.command?.includes(markerText) && predicate(entry))) return;
      await sleep(250);
    }
    fail("expected command audit was not observed");
  }
  async function kill(session, endpoint = origin) {
    const current = (await detail(session, endpoint)).session;
    if (current.status !== "active") {
      if (current.status === "killed" && current.kill_acknowledged_at) return current;
      return fail("kill target is no longer active; no owner acknowledgement proven");
    }
    const ack = await request(`/bastion/sessions/${session.id}/kill`, { method: "POST" }, endpoint);
    if (Number(ack?.id) !== session.id || ack.requested !== true || !ack.request_id ||
        typeof ack.killed !== "boolean") fail("invalid kill request acknowledgement");
    // HTTP 202/pending is valid; only final owner acknowledgement proves a kill.
    const ended = await waitFor(session, (row) => row.status !== "active", 20000, endpoint);
    if (ended.status !== "killed" || !ended.kill_acknowledged_at || ended.kill_request_id !== ack.request_id) {
      fail("kill was not finalized and acknowledged by the original owner");
    }
    return ended;
  }
  async function recording(session, expected, endpoint = origin) {
    const value = await detail(session, endpoint);
    if (value.session.status === "active" || value.session.recording_state !== "ready" || value.session.has_recording !== true) {
      fail("finalized recording is not readable in session metadata");
    }
    let response;
    try {
      response = await fetch(`${endpoint}/api/v1/bastion/sessions/${session.id}/recording`, {
        redirect: "error", headers: { authorization: `Bearer ${token}` }, signal: AbortSignal.timeout(15000),
      });
    } catch { return fail("recording transport failed"); }
    if (!response.ok) fail(`recording download returned HTTP ${response.status}`);
    const reader = response.body.getReader();
    const chunks = [];
    let size = 0;
    try {
      for (;;) {
        const { done, value: chunk } = await reader.read();
        if (done) break;
        size += chunk.length;
        if (size > 2 * 1024 * 1024) fail("recording exceeds the bounded acceptance size");
        chunks.push(Buffer.from(chunk));
      }
    } finally { await reader.cancel().catch(() => {}); }
    let events;
    try { events = Buffer.concat(chunks).toString("utf8").trim().split(/\r?\n/).map((line) => JSON.parse(line)); }
    catch { return fail("recording is not valid newline-delimited JSON"); }
    const header = events.shift();
    if (header?.version !== 2 || !(header.width > 0) || !(header.height > 0)) fail("invalid asciicast v2 header");
    let lastTime = 0;
    for (const event of events) {
      if (!Array.isArray(event) || event.length !== 3 || !Number.isFinite(event[0]) || event[0] < lastTime ||
          !["i", "o"].includes(event[1]) || typeof event[2] !== "string") fail("invalid asciicast event");
      lastTime = event[0];
    }
    const output = events.filter((event) => event[1] === "o").map((event) => event[2]).join("").replace(/\r/g, "");
    if (!output.includes(`\n${expected}\n`)) fail("recording lacks the exercised command output");
    return Buffer.concat(chunks);
  }
  function ownsFilter(row) {
    return row?.name === marker && row.pattern === marker && row.action === "deny" && row.priority === 1 && row.enabled === true;
  }
  async function cleanup() {
    let sessionsClean = true;
    let filterClean = !filterAttempted;
    for (const session of owned) {
      try {
        if (session.ws.readyState === WebSocket.OPEN || session.ws.readyState === WebSocket.CONNECTING) session.ws.close();
        if (!session.id) {
          sessionsClean = false;
          gaps.push("session setup had no attributable ID; fixture session sweep is required");
          continue;
        }
        const ended = await waitFor(session, (row) => row.status !== "active", 5000).catch(() => null);
        if (!ended) await kill(session);
        const final = (await detail(session)).session;
        if (!["closed", "killed", "failed"].includes(final.status)) fail("session has no recognized terminal state");
      } catch {
        sessionsClean = false;
        gaps.push(`cleanup/finalization failed for owned session ${session.id || "without-id"}`);
      }
    }
    if (filterAttempted) {
      try {
        const filters = await all("/bastion/filters");
        const matches = filters.filter((row) => row.name === marker);
        if (matches.length > 1 || matches.some((row) => !ownsFilter(row) || (filter?.id && row.id !== filter.id))) fail("filter ownership is ambiguous");
        if (!matches.length && (!filterConfirmed || filters.some((row) => row.id === filter?.id))) fail("attempted filter create cannot be reconciled");
        if (matches.length === 1) {
          filter = matches[0];
          if (!sessionsClean) fail("filter preserved while sessions remain unresolved");
          await request(`/bastion/filters/${idOf(filter.id) || fail("invalid filter ID")}`, { method: "DELETE" });
          if ((await all("/bastion/filters")).some((row) => row.name === marker || row.id === filter.id)) fail("filter deletion was not verified");
        }
        filterClean = true;
      } catch { gaps.push(`temporary deny filter cleanup unresolved (name=${marker}, id=${idOf(filter?.id)})`); }
    }
    return sessionsClean && filterClean;
  }
  try {
    if (!liveExec && !token) fail("read-only inventory requires MATRIX_ACCEPTANCE_TOKEN; credential lookup is disabled without live execution");
    const replicas = replicaOrigins();
    let health;
    try { health = await fetch(`${origin}/healthz`, { redirect: "error", signal: AbortSignal.timeout(8000) }); }
    catch { fail("live Matrix health check failed"); }
    if (!health.ok) fail("live Matrix health check failed");
    await authenticate();
    console.log("LIVE health and authentication: passed");
    const [grants, assets, accounts, rules, filters, sessions] = await Promise.all([
      all("/bastion/my/assets"), all("/bastion/assets"), all("/bastion/accounts"),
      all("/bastion/rules"), all("/bastion/filters"), all("/bastion/sessions"),
    ]);
    const sshGrants = grants.filter((row) => row.protocol === "ssh");
    console.log(`LIVE inventory: SSH grants=${sshGrants.length}; assets=${assets.length}; accounts=${accounts.length}; rules=${rules.length}; filters=${filters.length}; sessions=${sessions.length}`);
    const requestedAsset = process.env.MATRIX_BASTION_ASSET_ID;
    const requestedAccount = process.env.MATRIX_BASTION_ACCOUNT_ID;
    if ((requestedAsset && !idOf(requestedAsset)) || (requestedAccount && !idOf(requestedAccount)) ||
        (requestedAccount && !requestedAsset)) fail("explicit selection requires valid asset/account IDs");
    const candidates = sshGrants.filter((row) => (
      requestedAsset ? String(row.asset_id) === requestedAsset && /^192\.168\.6\.\d+$/.test(row.host) : row.host === targetHost
    ) && (!requestedAccount || String(row.account_id) === requestedAccount));
    if (candidates.length !== 1) fail("select exactly one authorized cluster-node asset/account; missing or ambiguous grant");
    const grant = candidates[0];
    if (!idOf(grant.asset_id) || !idOf(grant.account_id)) fail("invalid grant identity");
    const expectedUser = process.env.MATRIX_BASTION_EXPECTED_USER;
    if (expectedUser && (!/^mxfx[a-f0-9]{12}$/.test(expectedUser) || grant.username !== expectedUser)) fail("fixture grant does not identify the expected unprivileged user");
    console.log(`SELECTED asset_id=${grant.asset_id}; account_id=${grant.account_id}`);
    if (!liveExec) {
      gaps.push("read-only inventory only; SSH, command audit, owner acknowledgement and recordings remain unverified");
    } else {
      if (typeof WebSocket !== "function") fail("Node must provide the built-in WebSocket");
      for (const endpoint of replicas) {
        const otherActor = await request("/auth/me", {}, endpoint);
        if (Number(otherActor?.user_id) !== Number(actor.user_id)) fail("replica forward does not authenticate the same actor");
      }
      if (filters.some((row) => row.name === marker)) fail("temporary filter identity already exists");
      console.log(`OWNED Bastion filter intent: name=${marker}`);
      filterAttempted = true;
      filter = await request("/bastion/filters", {
        method: "POST", body: JSON.stringify({ name: marker, pattern: marker, action: "deny", priority: 1, enabled: true }),
      });
      if (!idOf(filter?.id) || !ownsFilter(filter)) fail("temporary filter contract was not preserved");
      filterConfirmed = true;
      console.log(`OWNED Bastion filter: id=${filter.id}; name=${marker}`);
      const first = await openSession(grant, replicas[0] || origin);
      let second;
      for (let attempt = 0; attempt < (replicas.length ? 1 : 3); attempt += 1) {
        const candidate = await openSession(grant, replicas[1] || origin);
        if (candidate.ownerId !== first.ownerId) { second = candidate; break; }
        candidate.ws.close();
        await waitFor(candidate, (row) => row.status !== "active");
      }
      if (!second) gaps.push("bounded session probing did not produce two distinct active owners");

      const nonce = randomBytes(8).toString("hex");
      const expected = `matrix_output_${nonce}`;
      if (expectedUser) {
        await outputCommand(first,
          `if [ "$(id -u)" -gt 0 ] && [ "$(id -g)" -gt 0 ] && [ "$(id -G)" = "$(id -g)" ]; then printf '\\n%s%s\\n' 'matrix_login_' "$(id -un)"; fi`,
          `matrix_login_${expectedUser}`);
      }
      // The expected output is split in the source command, so terminal echo cannot pass.
      await outputCommand(first, `printf '\\n%s%s\\n' 'matrix_output_' '${nonce}'`, expected);
      await audited(first, nonce, (entry) => entry.delivery_state === "forwarded");
      const denied = `${marker}_denied`;
      first.ws.send(JSON.stringify({ type: "input", data: `printf '\\n%s\\n' '${denied}'\r` }));
      await audited(first, denied, (entry) => entry.action === "deny" && entry.delivery_state === "blocked" && Number(entry.rule_id) === Number(filter.id));
      const afterDeny = `matrix_after_${nonce}`;
      await outputCommand(first, `printf '\\n%s%s\\n' 'matrix_after_' '${nonce}'`, afterDeny);
      if (first.output.replace(/\r/g, "").includes(`\n${denied}\n`)) fail("denied command produced execution output");

      if (second) {
        const secondExpected = `matrix_second_${nonce}`;
        await outputCommand(second, `printf '\\n%s%s\\n' 'matrix_second_' '${nonce}'`, secondExpected);
        await kill(second, replicas[0] || origin);
        await recording(second, secondExpected, replicas[0] || origin);
      }
      first.ws.close();
      const ended = await waitFor(first, (row) => row.status !== "active");
      if (ended.status !== "closed") fail("normal WebSocket close did not finalize a closed session");
      const original = await recording(first, expected, replicas[0] || origin);
      if (replicas.length && second) {
        const remote = await recording(first, expected, replicas[1]);
        if (!original.equals(remote)) fail("different Pod served different finalized recording bytes");
        console.log("FIXED-POD checks: distinct owners, cross-owner kill acknowledgement and identical cross-Pod recording passed.");
      } else {
        gaps.push("public load balancer cannot prove cross-Pod kill routing or recording reads; supply two fixed-Pod loopback origins");
      }
    }
  } catch (error) {
    const message = error instanceof AcceptanceFailure ? error.message : "live acceptance failed (runtime details suppressed)";
    gaps.push(message.slice(0, 240));
  } finally {
    if (await cleanup()) console.log("CLEANUP Bastion owned sessions/filter: complete");
    token = "";
  }
  if (gaps.length) {
    for (const gap of gaps) console.log(`GAP: ${gap}`);
    process.exitCode = 1;
  } else {
    console.log("LIVE Bastion acceptance checks passed for the exercised paths.");
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await runLiveAcceptance();
}
