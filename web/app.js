/* proot frontend — vanilla JS, no build step. */
(() => {
"use strict";

const $ = (s) => document.querySelector(s);

/* ---------- auth ---------- */

let token = "";
{
  const m = location.hash.match(/[#&]t=([0-9a-f]+)/i);
  if (m) {
    token = m[1];
    localStorage.setItem("proot_token", token);
    history.replaceState(null, "", location.pathname); // don't leave it in the URL bar
  } else {
    token = localStorage.getItem("proot_token") || "";
  }
}

function showGate() {
  $("#gate").classList.remove("hidden");
  document.querySelector("main").classList.add("hidden");
  $("#btn-add").classList.add("hidden");
}
$("#gate-form").addEventListener("submit", (e) => {
  e.preventDefault();
  token = $("#gate-token").value.trim();
  localStorage.setItem("proot_token", token);
  location.reload();
});

async function api(path, opts = {}) {
  opts.headers = Object.assign({ Authorization: "Bearer " + token }, opts.headers);
  const res = await fetch(path, opts);
  if (res.status === 401) { showGate(); throw new Error("unauthorized"); }
  if (!res.ok) {
    let msg = res.statusText;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  return res;
}

/* ---------- status stream ---------- */

let snap = null;
let evws = null;
let evRetry = 0;

function connectEvents() {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  evws = new WebSocket(`${proto}://${location.host}/api/events?token=${token}`);
  evws.onopen = () => {
    evRetry = 0;
    setConn(true);
  };
  evws.onmessage = (e) => {
    snap = JSON.parse(e.data);
    render();
  };
  evws.onclose = async (e) => {
    setConn(false);
    // A close right after open with no data usually means bad token.
    try { await api("/api/status"); } catch { return; }
    setTimeout(connectEvents, Math.min(15000, 500 * 2 ** evRetry++));
  };
}

function setConn(on) {
  const pill = $("#conn");
  pill.textContent = on ? "live" : "reconnecting…";
  pill.classList.toggle("on", on);
  pill.classList.toggle("off", !on);
  document.querySelector("main").classList.toggle("stale", !on);
}

/* ---------- rendering ---------- */

const STATUS_LABEL = {
  running: "running", exited: "exited", crashed: "crashed",
  crashloop: "crash loop", stopped: "stopped", killed: "killed by OS",
  created: "not started",
};

function fmtUptime(s) {
  if (s < 90) return s + "s";
  if (s < 5400) return Math.round(s / 60) + "m";
  if (s < 172800) return Math.round(s / 3600) + "h";
  return Math.round(s / 86400) + "d";
}

function esc(s) {
  return (s || "").replace(/[&<>"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

function appMeta(a) {
  const bits = [];
  if (a.status === "running") bits.push("up " + fmtUptime(a.uptime_sec));
  if (a.status === "crashed" && a.exit_code != null) bits.push("exit " + a.exit_code);
  if (a.signal) bits.push(a.signal);
  if (a.restart_count > 0) bits.push("↻ " + a.restart_count);
  if (a.autorestart !== "off") bits.push("auto: " + a.autorestart);
  return bits.join(" · ");
}

function render() {
  if (!snap) return;

  // killed-by-OS banner: the signature feature
  const killed = snap.killed || [];
  $("#banner").classList.toggle("hidden", killed.length === 0);
  if (killed.length) {
    $("#banner-text").textContent =
      `The OS killed ${killed.length} app${killed.length > 1 ? "s" : ""} ` +
      `(${killed.join(", ")}) — probably Android reclaiming memory.`;
  }

  const apps = snap.apps || [];
  $("#apps-empty").classList.toggle("hidden", apps.length > 0);
  $("#apps").innerHTML = apps.map((a) => `
    <div class="card" data-app="${esc(a.id)}">
      <div class="top">
        <div class="dot ${a.status}"></div>
        <div class="name">${esc(a.name)}</div>
        <span class="badge ${a.status}">${STATUS_LABEL[a.status] || a.status}</span>
        <div class="btns">
          ${a.status === "running"
            ? `<button data-act="stop" title="Stop">■</button>`
            : `<button data-act="start" title="Start">▶</button>`}
          <button data-act="restart" title="Restart">⟳</button>
          <button data-act="menu" title="More">⋮</button>
        </div>
      </div>
      ${a.last_line ? `<div class="last">${esc(a.last_line)}</div>` : ""}
      <div class="meta">${esc(appMeta(a))}</div>
    </div>`).join("");

  const un = snap.unmanaged || [];
  $("#unmanaged-section").classList.toggle("hidden", un.length === 0);
  $("#unmanaged").innerHTML = un.map((p) => `
    <div class="card unmanaged" data-pane="${esc(p.pane_id)}" data-title="${esc(p.session + ":" + p.window_name)}">
      <div class="top">
        <div class="dot ${p.dead ? "stopped" : "running"}"></div>
        <div class="name">${esc(p.session)} : ${esc(p.window_name)}</div>
        <span class="badge">${esc(p.command)}</span>
      </div>
    </div>`).join("");
}

/* ---------- card actions ---------- */

async function act(id, action, extra = {}) {
  try {
    await api(`/api/apps/${id}/action`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(Object.assign({ action }, extra)),
    });
  } catch (e) { alert(e.message); }
}

$("#apps").addEventListener("click", (e) => {
  const card = e.target.closest(".card");
  if (!card) return;
  const id = card.dataset.app;
  const btn = e.target.closest("button");
  if (!btn) { openTermForApp(id); return; }
  const a = (snap.apps || []).find((x) => x.id === id);
  if (btn.dataset.act === "menu") openAppSheet(a);
  else act(id, btn.dataset.act);
});

$("#unmanaged").addEventListener("click", (e) => {
  const card = e.target.closest(".card");
  if (card) openTerm(card.dataset.pane, card.dataset.title, null);
});

$("#btn-restart-killed").addEventListener("click", async () => {
  try { await api("/api/restart-killed", { method: "POST" }); } catch (e) { alert(e.message); }
});

/* ---------- app action sheet ---------- */

let sheetApp = null;

function openAppSheet(a) {
  sheetApp = a;
  $("#sheet-title").textContent = a.name;
  $("#sheet-sub").textContent = a.cmd + (a.cwd ? "  (in " + a.cwd + ")" : "");
  $("#sheet-autorestart").value = a.autorestart;
  const actions = [
    ["Open terminal", () => { closeModals(); openTermForApp(a.id); }],
    ["Restart", () => { act(a.id, "restart"); closeModals(); }],
    a.status === "running"
      ? ["Send Ctrl-C (SIGINT)", () => act(a.id, "signal", { signal: "INT" })]
      : ["Start", () => { act(a.id, "start"); closeModals(); }],
    ["Stop", () => { act(a.id, "stop"); closeModals(); }],
  ];
  if (a.has_crash_log) actions.push(["Last words (crash log)", () => showCrashLog(a.id)]);
  actions.push([a.pinned ? "Unpin" : "Pin to top",
    () => { patchApp(a.id, { pinned: !a.pinned }); closeModals(); }]);
  actions.push(["Delete app…", async () => {
    if (confirm(`Delete "${a.name}"? This stops it and removes its definition.`)) {
      try { await api(`/api/apps/${a.id}`, { method: "DELETE" }); } catch (e) { alert(e.message); }
      closeModals();
    }
  }]);
  $("#sheet-actions").innerHTML = "";
  for (const [label, fn] of actions) {
    const b = document.createElement("button");
    b.textContent = label;
    if (label.startsWith("Delete")) b.classList.add("danger");
    b.addEventListener("click", fn);
    $("#sheet-actions").appendChild(b);
  }
  $("#modal-app").classList.remove("hidden");
}

$("#sheet-autorestart").addEventListener("change", (e) => {
  if (sheetApp) patchApp(sheetApp.id, { autorestart: e.target.value });
});

async function patchApp(id, patch) {
  try {
    await api(`/api/apps/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    });
  } catch (e) { alert(e.message); }
}

async function showCrashLog(id) {
  try {
    const res = await api(`/api/apps/${id}/crash`);
    $("#crash-text").textContent = await res.text();
    $("#modal-crash").classList.remove("hidden");
  } catch (e) { alert(e.message); }
}

/* ---------- new app ---------- */

$("#btn-add").addEventListener("click", () => $("#modal-add").classList.remove("hidden"));

$("#form-add").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  try {
    await api("/api/apps", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        name: f.get("name"), cmd: f.get("cmd"),
        cwd: f.get("cwd") || "", autorestart: f.get("autorestart"),
      }),
    });
    e.target.reset();
    closeModals();
  } catch (err) { alert(err.message); }
});

function closeModals() {
  document.querySelectorAll(".overlay:not(#term-view)").forEach((el) => el.classList.add("hidden"));
}
document.querySelectorAll("[data-close]").forEach((b) => b.addEventListener("click", closeModals));
document.querySelectorAll(".overlay:not(#term-view)").forEach((el) =>
  el.addEventListener("click", (e) => { if (e.target === el) closeModals(); }));

/* ---------- terminal ---------- */

let term = null, fit = null, termWS = null, termApp = null;
let ctrlArmed = false, shiftArmed = false;

function openTermForApp(id) {
  const a = (snap.apps || []).find((x) => x.id === id);
  if (!a) return;
  if (!a.pane_id) {
    // No live pane (never started, or window gone). Offer to start.
    if (confirm(`"${a.name}" has no window right now. Start it?`)) act(id, "start");
    return;
  }
  openTerm(a.pane_id, a.name, a);
}

function openTerm(paneID, title, app) {
  termApp = app;
  $("#term-title").textContent = title;
  $("#term-menu").classList.toggle("hidden", !app);
  $("#term-view").classList.remove("hidden");

  term = new Terminal({
    theme: { background: "#000000" },
    fontSize: 13,
    fontFamily: "ui-monospace, Menlo, monospace",
    cursorBlink: true,
    scrollback: 3000,
  });
  fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open($("#term"));
  fit.fit();

  const proto = location.protocol === "https:" ? "wss" : "ws";
  termWS = new WebSocket(`${proto}://${location.host}/api/term?pane=${encodeURIComponent(paneID)}&token=${token}`);
  termWS.onopen = () => {
    $("#term-status").className = "dot running";
    sendResize();
  };
  termWS.onmessage = (e) => {
    const msg = JSON.parse(e.data);
    if (msg.t === "data") term.write(msg.data);
  };
  termWS.onclose = () => { $("#term-status").className = "dot stopped"; };

  term.onData((data) => {
    if (ctrlArmed && data.length === 1) {
      const c = data.toUpperCase().charCodeAt(0);
      if (c >= 64 && c <= 95) data = String.fromCharCode(c - 64);
      setCtrl(false);
    } else if (shiftArmed && data.length === 1) {
      data = data.toUpperCase();
      setShift(false);
    }
    if (termWS.readyState === 1) termWS.send(JSON.stringify({ t: "input", data }));
  });
  window.addEventListener("resize", onWinResize);
  setTimeout(() => { fit.fit(); sendResize(); term.focus(); }, 60);
}

function onWinResize() {
  if (!fit) return;
  fit.fit();
  sendResize();
}

function sendResize() {
  if (term && termWS && termWS.readyState === 1) {
    termWS.send(JSON.stringify({ t: "resize", cols: term.cols, rows: term.rows }));
  }
}

function closeTerm() {
  window.removeEventListener("resize", onWinResize);
  if (termWS) termWS.close();
  if (term) term.dispose();
  term = fit = termWS = termApp = null;
  setCtrl(false);
  setShift(false);
  $("#term-view").classList.add("hidden");
}

$("#term-back").addEventListener("click", closeTerm);
$("#term-menu").addEventListener("click", () => {
  if (!termApp) return;
  const a = (snap.apps || []).find((x) => x.id === termApp.id) || termApp;
  openAppSheet(a);
});

function setCtrl(v) {
  ctrlArmed = v;
  $("#key-ctrl").classList.toggle("armed", v);
}
function setShift(v) {
  shiftArmed = v;
  $("#key-shift").classList.toggle("armed", v);
}
$("#key-ctrl").addEventListener("click", () => { setCtrl(!ctrlArmed); term && term.focus(); });
$("#key-shift").addEventListener("click", () => { setShift(!shiftArmed); term && term.focus(); });

document.querySelectorAll(".keybar button[data-key]").forEach((b) =>
  b.addEventListener("click", () => {
    let key = b.dataset.key;
    // Armed modifiers turn named keys into their tmux chord form
    // (S-Up for shift-arrow selection, C-Left for word jumps, S-Tab = BTab).
    if (shiftArmed && /^(Up|Down|Left|Right)$/.test(key)) { key = "S-" + key; setShift(false); }
    else if (shiftArmed && key === "Tab") { key = "BTab"; setShift(false); }
    else if (ctrlArmed && /^(Up|Down|Left|Right)$/.test(key)) { key = "C-" + key; setCtrl(false); }
    if (termWS && termWS.readyState === 1) {
      termWS.send(JSON.stringify({ t: "key", key }));
    }
    term && term.focus();
  }));

/* ---------- boot ---------- */

if (!token) showGate();
else connectEvents();

})();
