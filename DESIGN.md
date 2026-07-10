# proot — Design Document

> Named after its home: the repo (and the proot-distro environment it's built for).
> Developed under the working name `muxboard`; alternatives considered: `pocketd`,
> `muxa`. See [Naming](#naming).

**One-liner:** pm2's dashboard with tmux's interactivity — a web control panel for
everything running on your phone server (or any Linux box), installable in one command,
usable from any browser.

## The problem

People run bots, scrapers, scripts, and servers inside tmux on Android phones
(Termux + proot-distro) and cheap VPSes. Today they have two bad options:

- **tmux alone** — fully interactive, but no supervision. Nothing tells you what
  crashed, when, or why. A crashed window just disappears, taking its last error
  output with it. Navigating tmux from a phone keyboard (`Ctrl-b` chords) is miserable.
- **Supervisors (pm2, supervisord, systemd)** — great supervision, but processes run
  detached. You can't type into a REPL, answer a prompt, or drive a menu-based script.
  And none of them exist or work well inside a proot userland with no root and no systemd.

Nothing marries the two. That's the gap.

## Target user & scope

- **One user, one box.** No accounts, no multi-tenancy, no RBAC. Ever, for v1.
- **Primary target:** proot-distro (Debian/Ubuntu) inside Termux on Android.
- **Secondary target (free by design):** any normal Linux box. proot *is* a normal
  userland, so the same binary runs anywhere. Positioning: *"tmux dashboard for any
  Linux box, designed to survive on a phone."* The Termux community is the beachhead,
  not the ceiling.
- **Open source.** Install experience is the product: every dependency we don't have
  is a GitHub issue we never get.

## Architecture

```
Phone/PC browser (PWA: dashboard + xterm.js)
        │  HTTPS + WebSocket (bearer token)
Tailscale / cloudflared / SSH tunnel     ← "access from anywhere", not our code
        │
proot agent (single Go binary, runs inside proot)
        │  tmux control mode (-C), pipe-pane, send-keys, capture-pane
tmux server ── one window per app ── wrapper ── user's program
```

### Core principles

1. **tmux is the engine, never the interface.** The agent orchestrates tmux; the user
   never needs to know `Ctrl-b` exists. Navigation is browser-side. But everything
   remains plain tmux underneath: kill the agent and your apps keep running; you can
   always `tmux attach` over SSH as a fallback.
2. **The agent runs inside proot, next to tmux.** The Termux layer (wake-lock,
   Termux:Boot autostart, optional tunnel daemon) is the installer's job, not the
   agent's.
3. **Event-driven, not polling.** proot traces every syscall, so CPU cost is real.
   Subscribe to tmux control mode events; don't run `list-panes` in a loop.
4. **Single static binary, web UI embedded** (Go `embed`). No npm at install time,
   no reverse proxy, no database. Cross-compiled for arm64 + amd64.
5. **Stay useful for panes we didn't create.** Unmanaged tmux windows appear on the
   dashboard too (dimmer section), attachable, with an "adopt" button.

### Language: Go

- Single static binary → `curl | sh` install with zero runtime deps in proot.
- Cheap goroutine-per-connection WebSocket handling.
- `embed` for the frontend.
- (Node was considered — Termux packages it — but Go wins on install friction and
  idle footprint.)

## Process supervision (the heart of the product)

### The wrapper

Every managed app runs inside a wrapper, not bare:

```
tmux new-window -n myapp 'proot run --id myapp -- python bot.py'
```

`proot run` (same binary, subcommand):

- On launch: writes `~/.proot/state/myapp.json` → `{pid, started_at}`.
- Runs the child, waits, then records `{exit_code, signal, ended_at}`.
- **Keeps the pane alive after exit**, showing
  `exited (code 1) — press r to restart` — instead of the pane vanishing along
  with the last error output. Major UX win over raw tmux.
- On non-zero exit: snapshots the last ~200 lines of the pane
  (`tmux capture-pane`) into the state dir as the crash's "last words."
  ~10 KB per crash, linked from the crash badge.

### App states — certainty, not heuristics

| State | Evidence |
|---|---|
| Running | pane alive, pid alive, no `ended_at` |
| Exited clean | `exit_code == 0` |
| Crashed | `exit_code != 0` or terminated by signal |
| **Killed by Android** | on agent startup: state file has live-looking pid that no longer exists and no `ended_at` |

The fourth state is the signature feature. After a Doze massacre the dashboard shows:
*"Android killed 4 apps at ~03:12. Restart all?"* — with an opt-in
"auto-resurrect on boot" toggle. No other tool reports this state.

### Restart policy

Per app: `off` / `on-crash` / `always`.

- Exponential backoff: 1s → 2s → 4s → … → 60s cap.
- Circuit breaker: crashed 5× within 2 minutes → give up, badge red.
  (Without this, a busted script in a restart loop cooks the phone battery —
  on this hardware that's a real bug, not a nitpick.)

## Web UI

Phone-first PWA; desktop is the same layout with room to breathe.

### Dashboard

- Vertical stack of **app cards**. Per card, by priority: status dot + name,
  live-updating one-line "last output" (often removes the need to open the terminal
  at all), uptime, restart count. Long-press/swipe → Restart / Stop / Pin.
- Second, dimmer section for **unmanaged tmux windows** with an "adopt" action
  (wraps them into managed apps on their next restart).
- Data is timestamped; when the WebSocket drops, cards grey out and a
  "reconnecting…" pill appears. A stale dashboard that *looks* live is the worst
  failure mode — phone browsers suspend tabs constantly.

### Terminal view

- xterm.js full-bleed. Output via `tmux pipe-pane`, input via `tmux send-keys` —
  full interactivity with no bespoke PTY layer.
- Fixed key bar above the Android keyboard: `Esc / Tab / Ctrl / Shift / ↑ ↓ ← → / Ctrl-C`,
  plus a `⋮` menu. Ctrl and Shift are sticky modifiers: arm one, then the next
  typed character or arrow gets the modifier (Shift+arrows → tmux `S-Up` etc.).
- **No tmux prefix keys in the UI, ever.**

### PWA

Installable (home-screen icon), dark theme by default, WebSocket auto-reconnect.

## Security

This is remote code execution by design. Non-negotiable floor:

- Bearer token generated on first run; the QR code shown in the terminal encodes
  `https://host:port/#token` — scan with the phone, you're in.
- Constant-time token comparison; WebSocket authenticates on connect, not just page load.
- **Binds to `127.0.0.1` by default.** `--listen 0.0.0.0` is an explicit opt-in.
- Remote access is delegated to Tailscale (`tailscaled --tun=userspace-networking`
  works rootless in Termux), cloudflared, or an SSH tunnel — documented, not reimplemented.

## Data model

Deliberately tiny. No SQLite.

- `~/.proot/apps.json` — `[{id, name, cmd, cwd, env, autorestart, pinned}]`.
  Editable over SSH when the UI is broken; backup = copy one file.
- `~/.proot/state/<id>.json` — runtime state, written by the wrapper.
  The state dir is the API's source of truth, so wrapper and agent stay decoupled:
  agent dies, wrappers keep recording faithfully.

## Install experience

1. **One-line installer** — `curl -fsSL … | sh` — detects Termux vs. proot vs.
   plain Linux and does the right thing for each: drop the binary, ensure tmux,
   set up the Termux side (wake-lock, battery-exemption prompt, Termux:Boot hook)
   when applicable, print the URL + QR.
2. **Zero config to first screen.** `proot serve` → QR code in the terminal →
   scan → dashboard. That moment is the README GIF that sells the project.

## v1 scope

1. `proot serve` — agent + embedded UI, QR + token on first run.
2. Read-only dashboard of all tmux windows (managed and not) with live status.
3. "New app" form (name, command, workdir, autorestart) → managed tmux window.
4. Terminal view with input and thumb-sized Restart / Stop / Ctrl-C.
5. Crash badge + last-words capture + auto-restart with backoff and circuit breaker.
6. "Killed by Android" detection + restart-all banner.
7. *(shipped in 0.2)* Adopt: turn an unmanaged window into a managed app. The
   form prefills from `pane_start_command` / `pane_current_path`; takeover
   (default) kills the old window and relaunches under the wrapper so the two
   copies never run concurrently, or save-only leaves the window untouched.

**Deferred to v2:** ntfy.sh crash notifications, resource graphs (CPU/RAM per app),
cron-style scheduled jobs in the UI, multi-box management, file browser.

Estimated size: ~2,000 lines of Go plus a modest vanilla/lightweight frontend.

## Android-specific landmines (tracked, not ignored)

- Doze/battery optimization kills background processes → wake-lock + exemption in
  installer, "killed by Android" state in product.
- proot syscall overhead → event-driven agent, no polling loops.
- Phone keyboards lack Esc/Ctrl → fixed key bar in terminal view.
- Termux `tmux` and proot-distro `tmux` are different servers — agent must talk to
  the one inside proot (where it runs). Document that apps live inside proot.

## Naming

Final name: **`proot`** — matches the repo and the environment it's built for
(proot-distro on Termux). Trade-off accepted deliberately: it collides with the
name of the proot emulator itself, but the emulator lives on the Termux side of
the wall while this binary lives inside the distro, so they never share a PATH
in the primary deployment.

History: developed under the working name `muxboard` (best search
discoverability). Other candidates: `pocketd` (best story — "a daemon in your
pocket"), `muxa` (best brand if it outgrows tmux internals).

## Open questions

- Frontend stack: vanilla + a micro-framework (e.g. Preact/Svelte) vs. plain JS.
  Constraint: must fit the "no npm at install time" rule via `embed`, which any of
  these satisfy at build time.
- ~~Should "adopt" be able to take over a running process (reptyr-style) or only
  re-wrap on next restart?~~ Resolved: adopt restarts the command under the
  wrapper (kill-then-start), never re-parents. reptyr-style attach stays out —
  it's fragile under proot and the ptrace dance isn't worth the edge cases.
- Token rotation / revocation UX beyond "delete the file and restart."
