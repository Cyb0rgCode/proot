# muxboard

**pm2's dashboard with tmux's interactivity.** A single-binary web control panel for
everything running on your phone server (Termux + proot) or any Linux box: see what's
running, what crashed and why, restart with one tap, and type into a real live
terminal — from any browser, phone or PC.

> ⚠️ Early stage — currently in design. Read the full [design document](DESIGN.md).

## Why

If you run bots and scripts in tmux on an Android phone or a cheap VPS, you know the pain:

- tmux gives you **interactivity but no supervision** — a crashed window just vanishes,
  along with its last error output.
- pm2/systemd give you **supervision but no interactivity** — and don't work in a
  rootless proot userland anyway.

muxboard marries the two, and adds the one thing nothing else can tell you:
**"Android killed your apps at 03:12 — restart all?"**

## Planned v1

- One-line install, zero config: `muxboard serve` prints a QR code — scan it, you're in.
- Phone-first dashboard: a card per app with live status, last output, uptime, crash badge.
- Full interactive terminal (xterm.js) with thumb-sized Ctrl-C / Restart / arrow keys.
- Crash detection with exit codes, "last words" capture, auto-restart with backoff.
- Everything stays plain tmux underneath — kill muxboard, your apps keep running.

## Status

Design phase. See [DESIGN.md](DESIGN.md) for architecture, scope, and open questions.
