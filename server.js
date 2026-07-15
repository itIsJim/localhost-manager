#!/usr/bin/env node
/**
 * local-port-manager — see and manage locally listening TCP ports.
 *
 * Zero dependencies. Scans with `lsof` + `ps` (macOS/Linux), classifies each
 * listener as active / pending / stale, and lets you kill the owning process.
 *
 *   active  — listening and currently in use (has established connections)
 *   pending — listening but not used yet (no connections; may still be
 *             compiling / warming up)
 *   stale   — process has been up longer than STALE_HOURS, or the port is
 *             stuck (accepts nothing: our health probe can't connect)
 */

const http = require('http');
const net = require('net');
const os = require('os');
const path = require('path');
const fs = require('fs');
const { execFile } = require('child_process');

const PORT = Number(process.env.PORT) || 4321;
const STALE_HOURS = Number(process.env.STALE_HOURS) || 24;
const PROBE_TIMEOUT_MS = 900;
const SELF_PID = process.pid;

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

function run(cmd, args) {
  return new Promise((resolve) => {
    execFile(cmd, args, { maxBuffer: 16 * 1024 * 1024 }, (err, stdout) => {
      // lsof exits 1 when some fds can't be read; stdout is still usable.
      resolve(stdout || '');
    });
  });
}

/** Parse `lsof -F pcnuL` machine output into records grouped by process. */
function parseLsofListen(out) {
  const rows = []; // { pid, command, user, addr, port }
  let cur = {};
  for (const line of out.split('\n')) {
    if (!line) continue;
    const tag = line[0];
    const val = line.slice(1);
    if (tag === 'p') cur = { pid: Number(val) };
    else if (tag === 'c') cur.command = val;
    else if (tag === 'L') cur.user = val;
    else if (tag === 'n') {
      // e.g. "*:3000", "127.0.0.1:8080", "[::1]:5432"
      const i = val.lastIndexOf(':');
      if (i === -1) continue;
      const addr = val.slice(0, i);
      const port = Number(val.slice(i + 1));
      if (!Number.isFinite(port)) continue;
      rows.push({ pid: cur.pid, command: cur.command, user: cur.user, addr, port });
    }
  }
  return rows;
}

/** Count established connections per local port. */
function parseLsofEstablished(out) {
  const counts = new Map(); // port -> count
  for (const line of out.split('\n')) {
    if (!line.startsWith('n')) continue;
    const local = line.slice(1).split('->')[0];
    const i = local.lastIndexOf(':');
    if (i === -1) continue;
    const port = Number(local.slice(i + 1));
    if (!Number.isFinite(port)) continue;
    counts.set(port, (counts.get(port) || 0) + 1);
  }
  return counts;
}

/** "[[dd-]hh:]mm:ss" -> seconds */
function parseEtime(etime) {
  if (!etime) return null;
  let days = 0;
  let rest = etime.trim();
  const dash = rest.indexOf('-');
  if (dash !== -1) {
    days = Number(rest.slice(0, dash));
    rest = rest.slice(dash + 1);
  }
  const parts = rest.split(':').map(Number);
  if (parts.some(Number.isNaN)) return null;
  while (parts.length < 3) parts.unshift(0);
  const [h, m, s] = parts;
  return days * 86400 + h * 3600 + m * 60 + s;
}

async function getProcessInfo(pids) {
  if (pids.length === 0) return new Map();
  const out = await run('ps', ['-p', pids.join(','), '-o', 'pid=,etime=,%cpu=,args=']);
  const info = new Map();
  for (const line of out.split('\n')) {
    const m = line.match(/^\s*(\d+)\s+(\S+)\s+(\S+)\s+(.*)$/);
    if (!m) continue;
    info.set(Number(m[1]), {
      uptimeSec: parseEtime(m[2]),
      cpu: Number(m[3]) || 0,
      args: m[4],
    });
  }
  return info;
}

/** TCP connect probe: 'open' | 'stuck' */
function probe(addr, port) {
  const host = addr === '*' || addr === '' ? '127.0.0.1' : addr.replace(/^\[|\]$/g, '');
  return new Promise((resolve) => {
    const sock = net.connect({ host, port, timeout: PROBE_TIMEOUT_MS });
    const done = (result) => {
      sock.destroy();
      resolve(result);
    };
    sock.once('connect', () => done('open'));
    sock.once('timeout', () => done('stuck'));
    sock.once('error', () => done('stuck'));
  });
}

function classify({ uptimeSec, probeResult, establishedCount }) {
  const reasons = [];
  if (uptimeSec !== null && uptimeSec > STALE_HOURS * 3600) {
    reasons.push(`process up for over ${STALE_HOURS}h`);
  }
  if (probeResult === 'stuck') {
    reasons.push('port not accepting connections (stuck)');
  }
  if (reasons.length > 0) return { status: 'stale', reasons };
  if (establishedCount > 0) {
    return { status: 'active', reasons: [`${establishedCount} open connection(s)`] };
  }
  return { status: 'pending', reasons: ['listening but no connections yet'] };
}

async function scanPorts() {
  const [listenOut, estOut] = await Promise.all([
    run('lsof', ['-nP', '-iTCP', '-sTCP:LISTEN', '-F', 'pcnuL']),
    run('lsof', ['-nP', '-iTCP', '-sTCP:ESTABLISHED', '-F', 'pn']),
  ]);
  const listeners = parseLsofListen(listenOut);
  const established = parseLsofEstablished(estOut);

  // Dedupe: one row per (pid, port); merge bind addresses (IPv4 + IPv6 etc.)
  const byKey = new Map();
  for (const l of listeners) {
    const key = `${l.pid}:${l.port}`;
    if (byKey.has(key)) {
      const row = byKey.get(key);
      if (!row.addrs.includes(l.addr)) row.addrs.push(l.addr);
    } else {
      byKey.set(key, { ...l, addrs: [l.addr] });
    }
  }
  const rows = [...byKey.values()];

  const procInfo = await getProcessInfo([...new Set(rows.map((r) => r.pid))]);
  const probes = await Promise.all(rows.map((r) => probe(r.addrs[0], r.port)));

  const currentUser = os.userInfo().username;
  return rows
    .map((r, i) => {
      const p = procInfo.get(r.pid) || { uptimeSec: null, cpu: 0, args: '' };
      const establishedCount = established.get(r.port) || 0;
      const { status, reasons } = classify({
        uptimeSec: p.uptimeSec,
        probeResult: probes[i],
        establishedCount,
      });
      return {
        port: r.port,
        pid: r.pid,
        command: r.command,
        args: p.args,
        user: r.user,
        addrs: r.addrs,
        uptimeSec: p.uptimeSec,
        cpu: p.cpu,
        connections: establishedCount,
        status,
        reasons,
        self: r.pid === SELF_PID,
        killable: r.pid !== SELF_PID && r.user === currentUser && r.pid > 1,
      };
    })
    .sort((a, b) => a.port - b.port || a.pid - b.pid);
}

// ---------------------------------------------------------------------------
// Kill
// ---------------------------------------------------------------------------

async function killPid(pid, force) {
  const ports = await scanPorts();
  const row = ports.find((r) => r.pid === pid);
  if (!row) throw new Error(`PID ${pid} is not holding any listening port`);
  if (!row.killable) throw new Error(`PID ${pid} (${row.command}) is not killable from here`);
  process.kill(pid, force ? 'SIGKILL' : 'SIGTERM');
  // Report whether it actually died (give SIGTERM a moment).
  await new Promise((r) => setTimeout(r, 700));
  let alive = true;
  try {
    process.kill(pid, 0);
  } catch {
    alive = false;
  }
  return { pid, signal: force ? 'SIGKILL' : 'SIGTERM', alive };
}

// ---------------------------------------------------------------------------
// HTTP server
// ---------------------------------------------------------------------------

function json(res, code, body) {
  res.writeHead(code, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

const server = http.createServer(async (req, res) => {
  try {
    if (req.method === 'GET' && (req.url === '/' || req.url === '/index.html')) {
      const html = fs.readFileSync(path.join(__dirname, 'public', 'index.html'));
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
      res.end(html);
    } else if (req.method === 'GET' && req.url === '/api/ports') {
      json(res, 200, { staleHours: STALE_HOURS, selfPort: PORT, ports: await scanPorts() });
    } else if (req.method === 'POST' && req.url === '/api/kill') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const { pid, force } = JSON.parse(body || '{}');
      if (!Number.isInteger(pid) || pid <= 1) return json(res, 400, { error: 'invalid pid' });
      json(res, 200, await killPid(pid, Boolean(force)));
    } else {
      json(res, 404, { error: 'not found' });
    }
  } catch (err) {
    json(res, 500, { error: err.message });
  }
});

server.listen(PORT, '127.0.0.1', () => {
  console.log(`local-port-manager running at http://localhost:${PORT}`);
});
