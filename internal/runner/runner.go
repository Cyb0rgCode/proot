// Package runner implements `muxboard run --id <id>` — the wrapper that
// every managed app runs inside of. It is the only writer of the app's
// runtime state file, which is what makes the agent/wrapper split race-free:
// the agent can die and the wrapper keeps recording faithfully.
//
// The wrapper also keeps the pane alive after the app exits (showing the
// exit status and a restart prompt) instead of letting the window vanish
// along with the last error output, and implements the auto-restart policy
// locally so supervision works even with no agent running.
package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"muxboard/internal/config"
	"muxboard/internal/state"
	"muxboard/internal/tmuxctl"
)

const (
	breakerWindow = 2 * time.Minute
	breakerCount  = 5
	goodRun       = 60 * time.Second // a child that lived this long resets backoff
	maxBackoff    = 60 * time.Second
	crashLines    = 200
)

type wrapper struct {
	id       string
	store    *state.Store
	paneID   string
	windowID string

	mu sync.Mutex // guards state file writes between main loop and signal handler
	rs *state.RunState
}

// Main is the entry point for `muxboard run --id <id>`. It never returns
// except by exiting the process.
func Main(id string) int {
	if !state.ValidID(id) {
		fmt.Fprintf(os.Stderr, "muxboard run: invalid app id %q\n", id)
		return 2
	}
	if err := config.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "muxboard run: %v\n", err)
		return 1
	}
	w := &wrapper{id: id, store: state.NewStore()}
	w.paneID = os.Getenv("TMUX_PANE")
	if w.paneID != "" {
		if out, err := exec.Command("tmux", "display-message", "-p", "-t", w.paneID, "#{window_id}").Output(); err == nil {
			w.windowID = strings.TrimSpace(string(out))
		}
	}

	// tmux kill-window HUPs the pane's process group; record the death so
	// the agent can tell "closed" from "killed by Android" (which leaves
	// no ended_at behind).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sig
		w.recordEnd(nil, s.String())
		os.Exit(1)
	}()

	return w.loop()
}

func (w *wrapper) loop() int {
	backoffN := 0
	for {
		app, ok, err := w.store.Get(w.id)
		if err != nil || !ok {
			w.printf("app %q not found in apps.json (%v) — press q to close", w.id, err)
			if !w.waitKey(0) {
				return 1
			}
			continue
		}

		start := time.Now()
		exitCode, sigName, runErr := w.runChild(app)
		dur := time.Since(start)
		if dur >= goodRun {
			backoffN = 0
		}

		crashed := runErr != nil || exitCode != 0 || sigName != ""
		if crashed {
			w.captureCrash()
		}

		switch {
		case state.IsStopped(w.id):
			w.printf("stopped — press r to restart, q to close")
			if !w.waitKey(0) {
				return 0
			}
		case w.breakerTripped():
			w.setBreaker(true)
			w.printf("crash loop: %d starts in %s — giving up. press r to retry, q to close",
				breakerCount, breakerWindow)
			if !w.waitKey(0) {
				return 1
			}
			w.setBreaker(false)
		case app.Autorestart == "always" || (app.Autorestart == "on-crash" && crashed):
			backoffN++
			delay := time.Duration(1<<uint(min(backoffN-1, 6))) * time.Second
			if delay > maxBackoff {
				delay = maxBackoff
			}
			w.printf("%s — restarting in %s (r = now, q = close)", exitDesc(exitCode, sigName), delay)
			if !w.waitKey(delay) {
				return 0
			}
		default:
			w.printf("%s — press r to restart, q to close", exitDesc(exitCode, sigName))
			if !w.waitKey(0) {
				return boolToExit(crashed)
			}
		}
		state.SetStopped(w.id, false) // restarting implies un-stopping
	}
}

// runChild launches the app and blocks until it exits, keeping the state
// file current on both edges.
func (w *wrapper) runChild(app state.App) (exitCode int, sigName string, err error) {
	fmt.Printf("\x1b[2m[muxboard] starting %s: %s\x1b[0m\r\n", app.ID, app.Cmd)

	cmd := exec.Command("sh", "-c", app.Cmd)
	cmd.Dir = app.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	for k, v := range app.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Start(); err != nil {
		w.recordStart(0)
		code := 127
		w.recordEnd(&code, "")
		fmt.Fprintf(os.Stderr, "[muxboard] failed to start: %v\r\n", err)
		return 127, "", err
	}
	w.recordStart(cmd.Process.Pid)

	werr := cmd.Wait()
	var ee *exec.ExitError
	if errors.As(werr, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sigName = ws.Signal().String()
		}
		exitCode = ee.ExitCode()
	} else if werr != nil {
		exitCode = 1
	}
	w.recordEnd(&exitCode, sigName)
	return exitCode, sigName, nil
}

func (w *wrapper) recordStart(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	prev, _ := state.ReadRunState(w.id)
	rs := &state.RunState{
		ID: w.id, PID: pid, WrapperPID: os.Getpid(),
		WindowID: w.windowID, PaneID: w.paneID,
		StartedAt: time.Now(),
	}
	if prev != nil {
		rs.Launches = prev.Launches
	}
	rs.Launches = append(rs.Launches, rs.StartedAt)
	w.rs = rs
	state.WriteRunState(rs)
}

func (w *wrapper) recordEnd(exitCode *int, sigName string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rs == nil || w.rs.EndedAt != nil {
		return
	}
	now := time.Now()
	w.rs.EndedAt = &now
	w.rs.ExitCode = exitCode
	w.rs.Signal = sigName
	state.WriteRunState(w.rs)
}

func (w *wrapper) setBreaker(v bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rs == nil {
		return
	}
	w.rs.Breaker = v
	state.WriteRunState(w.rs)
}

func (w *wrapper) breakerTripped() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.rs == nil {
		return false
	}
	n := 0
	cutoff := time.Now().Add(-breakerWindow)
	for _, t := range w.rs.Launches {
		if t.After(cutoff) {
			n++
		}
	}
	return n >= breakerCount
}

// captureCrash snapshots the pane's last lines — the app's "last words".
func (w *wrapper) captureCrash() {
	if w.paneID == "" {
		return
	}
	if out, err := tmuxctl.Capture(w.paneID, crashLines, false); err == nil {
		hdr := fmt.Sprintf("# %s crashed at %s\n", w.id, time.Now().Format(time.RFC3339))
		os.WriteFile(config.CrashFile(w.id), []byte(hdr+out+"\n"), 0o600)
	}
}

// waitKey blocks until 'r' (restart, returns true) or 'q' (returns false).
// With timeout > 0 it auto-restarts (returns true) when the timer fires.
// The agent drives remote restarts by sending a literal 'r' into the pane.
//
// Go keeps fd 0 in blocking mode (SetReadDeadline is unsupported on it),
// so the timeout is implemented with poll(2) + a one-byte read.
func (w *wrapper) waitKey(timeout time.Duration) bool {
	fd := int(os.Stdin.Fd())
	oldState, rawErr := term.MakeRaw(fd)
	if rawErr == nil {
		defer term.Restore(fd, oldState)
	}
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return true
		}
		// A Stop from the UI during a backoff wait cancels the restart.
		if timeout > 0 && state.IsStopped(w.id) {
			w.printf("stopped — press r to restart, q to close")
			deadline = time.Time{}
			timeout = 0
		}
		b, ok := readByteTimeout(fd, 300*time.Millisecond)
		if !ok {
			continue
		}
		switch b {
		case 'r', 'R':
			return true
		case 'q', 'Q':
			return false
		}
	}
}

func readByteTimeout(fd int, timeout time.Duration) (byte, bool) {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(pfd, int(timeout.Milliseconds()))
	if err != nil || n == 0 || pfd[0].Revents&unix.POLLIN == 0 {
		return 0, false
	}
	var buf [1]byte
	m, err := unix.Read(fd, buf[:])
	if err != nil || m == 0 {
		return 0, false
	}
	return buf[0], true
}

func (w *wrapper) printf(format string, args ...any) {
	fmt.Printf("\r\n\x1b[7m[muxboard]\x1b[0m "+format+"\r\n", args...)
}

func exitDesc(code int, sig string) string {
	if sig != "" {
		return "terminated by signal " + sig
	}
	if code == 0 {
		return "exited cleanly (code 0)"
	}
	return fmt.Sprintf("\x1b[31mcrashed (code %d)\x1b[0m", code)
}

func boolToExit(crashed bool) int {
	if crashed {
		return 1
	}
	return 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
