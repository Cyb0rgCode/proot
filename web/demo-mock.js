/* proot demo mode — simulates the agent entirely in the browser.
 *
 * Loaded ONLY on the GitHub Pages demo (the deploy workflow injects this
 * script before app.js); a real proot install never serves it. It replaces
 * fetch() for /api/* and WebSocket for the events/terminal sockets with an
 * in-memory fake agent, so the UI behaves like the real thing: apps run,
 * crash, restart, get adopted, and stream terminal output.
 */
(() => {
"use strict";

localStorage.setItem("proot_token", "demo"); // skip the token gate

/* ---------- fake box state ---------- */

const now = () => new Date();
const iso = (d) => d.toISOString();
let paneSeq = 40;

const apps = {
  "tg-bot": {
    id: "tg-bot", name: "tg-bot", cmd: "python3 bot.py",
    cwd: "/root/tg-bot", autorestart: "on-crash", pinned: true,
    status: "running", pane_id: "%31", started: Date.now() - 86400e3 * 2.3,
    restart_count: 4, cpu: 2.1, mem: 48e6,
    lines: () => `update #${1200 + Math.floor((Date.now() / 3000) % 900)} handled in ${(Math.random() * 80 + 20) | 0}ms`,
  },
  "scraper": {
    id: "scraper", name: "scraper", cmd: "node scrape.js --daily",
    cwd: "/root/scraper", autorestart: "off", schedule: "0 3 * * *",
    status: "exited", pane_id: "%32", exit_code: 0,
    restart_count: 12, lines: () => "wrote 4,812 rows to data.sqlite",
  },
  "webhook": {
    id: "webhook", name: "webhook", cmd: "./server --port 3000",
    cwd: "/root/hooks", autorestart: "on-crash",
    status: "crashloop", pane_id: "%33", exit_code: 1, restart_count: 5,
    has_crash_log: true,
    lines: () => "panic: connection refused (db)",
  },
  "miner-canary": {
    id: "miner-canary", name: "miner-canary", cmd: "python3 watch.py",
    cwd: "/root", autorestart: "always",
    status: "killed", pane_id: "", restart_count: 1,
  },
};

const unmanaged = [
  { session: "work", window_id: "@9", window_name: "ssh-tunnel", pane_id: "%20",
    command: "ssh", start_command: "ssh -N -R 8022:localhost:22 vps", path: "/root", dead: false },
  { session: "work", window_id: "@10", window_name: "bash", pane_id: "%21",
    command: "bash", start_command: "", path: "/root", dead: false },
];

let settings = { ntfy_topic: "" };

const CRASH_LOG = `# webhook crashed at ${iso(now())}
[proot] starting webhook: ./server --port 3000
listening on :3000
POST /hook 200 12ms
POST /hook 200 9ms
db: dial tcp 127.0.0.1:5432: connect: connection refused
panic: connection refused (db)

goroutine 1 [running]:
main.mustDB(...)
\t/root/hooks/main.go:44
[proot] crashed (code 1) — restarting in 16s (r = now, q = close)`;

function snapshot() {
  const t = Date.now();
  const list = Object.values(apps).map((a) => {
    const running = a.status === "running";
    return {
      id: a.id, name: a.name, cmd: a.cmd, cwd: a.cwd,
      autorestart: a.autorestart, pinned: !!a.pinned, schedule: a.schedule || "",
      status: a.status, pane_id: a.pane_id,
      exit_code: a.exit_code ?? null, restart_count: a.restart_count || 0,
      uptime_sec: running ? Math.floor((t - a.started) / 1000) : 0,
      has_crash_log: !!a.has_crash_log,
      last_line: a.lines ? a.lines() : "",
      cpu_percent: running ? +(a.cpu + Math.random() * 0.8).toFixed(1) : 0,
      mem_bytes: running ? Math.floor(a.mem * (1 + Math.random() * 0.02)) : 0,
      next_run: a.schedule ? iso(nextThreeAM()) : undefined,
    };
  });
  return {
    apps: list, unmanaged,
    killed: Object.values(apps).filter((a) => a.status === "killed").map((a) => a.id),
    time: iso(now()),
  };
}

function nextThreeAM() {
  const d = new Date();
  d.setHours(3, 0, 0, 0);
  if (d <= new Date()) d.setDate(d.getDate() + 1);
  return d;
}

function startApp(a) {
  a.status = "running";
  a.started = Date.now();
  a.pane_id = "%" + paneSeq++;
  a.cpu = a.cpu || 1.2;
  a.mem = a.mem || 22e6;
  a.exit_code = null;
  if (!a.lines) a.lines = () => "[demo] doing important work…";
}

/* ---------- fetch interception ---------- */

const realFetch = window.fetch.bind(window);
const ok = (body, code = 200) =>
  Promise.resolve(new Response(JSON.stringify(body), {
    status: code, headers: { "Content-Type": "application/json" } }));

window.fetch = (url, opts = {}) => {
  const u = typeof url === "string" ? url : url.url;
  if (!u.startsWith("/api/")) return realFetch(url, opts);
  const body = opts.body ? JSON.parse(opts.body) : {};
  const method = (opts.method || "GET").toUpperCase();

  if (u === "/api/status" || u.startsWith("/api/status?")) return ok(snapshot());
  if (u === "/api/settings" && method === "GET") return ok(settings);
  if (u === "/api/settings" && method === "PUT") {
    settings = { ntfy_topic: body.ntfy_topic ? (body.ntfy_topic.startsWith("http") ?
      body.ntfy_topic : "https://ntfy.sh/" + body.ntfy_topic) : "" };
    return ok(settings);
  }
  if (u === "/api/apps" && method === "POST") {
    const id = body.name.toLowerCase().replace(/[^a-z0-9_-]+/g, "-");
    if (apps[id]) return ok({ error: `an app with id "${id}" already exists` }, 409);
    apps[id] = { id, name: body.name, cmd: body.cmd, cwd: body.cwd,
      autorestart: body.autorestart || "off", schedule: body.schedule || "", restart_count: 0 };
    startApp(apps[id]);
    return ok(apps[id], 201);
  }
  if (u === "/api/adopt" && method === "POST") {
    const p = unmanaged.find((x) => x.pane_id === body.pane_id);
    if (!p) return ok({ error: "pane not found" }, 404);
    const id = (body.name || p.window_name).toLowerCase().replace(/[^a-z0-9_-]+/g, "-");
    apps[id] = { id, name: body.name || p.window_name, cmd: body.cmd, cwd: body.cwd,
      autorestart: body.autorestart || "off", schedule: body.schedule || "", restart_count: 0 };
    if (body.takeover !== false) {
      unmanaged.splice(unmanaged.indexOf(p), 1);
      startApp(apps[id]);
    } else {
      apps[id].status = "created";
    }
    return ok({ app: apps[id], takeover: body.takeover !== false }, 201);
  }
  let m = u.match(/^\/api\/apps\/([^/]+)\/action$/);
  if (m && method === "POST") {
    const a = apps[m[1]];
    if (!a) return ok({ error: "no such app" }, 404);
    if (body.action === "stop") { a.status = "stopped"; a.exit_code = null; a.pane_id = "%" + paneSeq++; }
    if (body.action === "start" || body.action === "restart") {
      startApp(a);
      if (body.action === "restart") a.restart_count = (a.restart_count || 0) + 1;
    }
    if (body.action === "signal") a.status = "crashed", a.exit_code = 130;
    return ok({ ok: true });
  }
  m = u.match(/^\/api\/apps\/([^/]+)\/crash$/);
  if (m) return Promise.resolve(new Response(CRASH_LOG, { status: 200 }));
  m = u.match(/^\/api\/apps\/([^/]+)$/);
  if (m && method === "PATCH") { Object.assign(apps[m[1]] || {}, body); return ok(apps[m[1]]); }
  if (m && method === "DELETE") { delete apps[m[1]]; return ok({ ok: true }); }
  if (u === "/api/restart-killed" && method === "POST") {
    const ids = [];
    for (const a of Object.values(apps)) {
      if (a.status === "killed") { startApp(a); ids.push(a.id); }
    }
    return ok({ restarted: ids });
  }
  return ok({ error: "not in demo" }, 404);
};

/* ---------- WebSocket interception ---------- */

const RealWS = window.WebSocket;
class DemoWS {
  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this._timers = [];
    setTimeout(() => {
      this.onopen && this.onopen();
      if (url.includes("/api/events")) {
        const push = () => this.onmessage &&
          this.onmessage({ data: JSON.stringify(snapshot()) });
        push();
        this._timers.push(setInterval(push, 2000));
      } else if (url.includes("/api/term")) {
        this._term(url);
      }
    }, 60);
  }
  _term(url) {
    const pane = decodeURIComponent((url.match(/pane=([^&]+)/) || [])[1] || "");
    const app = Object.values(apps).find((a) => a.pane_id === pane);
    const send = (s) => this.onmessage &&
      this.onmessage({ data: JSON.stringify({ t: "data", data: s }) });
    send(`\x1b[2m[proot demo] simulated terminal — this pane is fake, the UI is real\x1b[0m\r\n`);
    if (app && app.status === "running" && app.lines) {
      send(`[proot] starting ${app.id}: ${app.cmd}\r\n`);
      this._timers.push(setInterval(() => send(app.lines() + "\r\n"), 1500));
    } else if (app && app.status === "crashloop") {
      send(CRASH_LOG.replace(/\n/g, "\r\n") + "\r\n");
    } else {
      send(`root@phone:~# `);
    }
  }
  send(raw) {
    const msg = JSON.parse(raw);
    const reply = (s) => this.onmessage &&
      this.onmessage({ data: JSON.stringify({ t: "data", data: s }) });
    if (msg.t === "input") {
      reply(msg.data === "\r" ? "\r\ndemo: commands aren't executed here — install proot for the real thing\r\nroot@phone:~# " : msg.data);
    }
    if (msg.t === "key" && msg.key === "C-c") reply("^C\r\nroot@phone:~# ");
  }
  close() {
    this._timers.forEach(clearInterval);
    this.readyState = 3;
    this.onclose && this.onclose({});
  }
}
window.WebSocket = function (url, protos) {
  return url.includes("/api/") ? new DemoWS(url) : new RealWS(url, protos);
};

/* ---------- demo ribbon ---------- */

addEventListener("DOMContentLoaded", () => {
  const bar = document.createElement("div");
  bar.innerHTML = `▶ Live demo with <b>simulated data</b> — everything you click works,
    nothing is real. <a href="https://github.com/Cyb0rgCode/proot">Install proot →</a>`;
  bar.style.cssText = "background:#1a2f52;color:#e6edf3;padding:8px 14px;" +
    "font-size:13px;text-align:center;border-bottom:1px solid #2d333b";
  bar.querySelector("a").style.color = "#58a6ff";
  document.body.prepend(bar);
});

})();
