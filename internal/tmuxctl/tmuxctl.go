// Package tmuxctl wraps the tmux CLI. proot never replaces tmux — it
// orchestrates it, so a user can always `tmux attach` as a fallback.
package tmuxctl

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// sep is the field separator for -F parsing. It must be printable ASCII:
// tmux octal-escapes control characters in format output, and non-ASCII
// gets replaced outright under a non-UTF-8 locale. A row whose names
// contain this sequence is skipped rather than misparsed.
const sep = ";|;"

// Pane is one tmux pane as seen by list-panes -a.
type Pane struct {
	Session    string `json:"session"`
	WindowID   string `json:"window_id"` // @n
	WindowName string `json:"window_name"`
	PaneID     string `json:"pane_id"` // %n
	Command    string `json:"command"` // pane_current_command
	Dead       bool   `json:"dead"`
	Width      int    `json:"-"`
	Height     int    `json:"-"`
}

func run(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).CombinedOutput()
	s := strings.TrimRight(string(out), "\n")
	if err != nil {
		return s, fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

// Raw runs an arbitrary tmux command (used for send-keys -H batches).
func Raw(args ...string) error {
	_, err := run(args...)
	return err
}

// Running reports whether a tmux server is reachable.
func Running() bool {
	_, err := run("list-sessions")
	return err == nil
}

func ListPanes() ([]Pane, error) {
	format := strings.Join([]string{
		"#{session_name}", "#{window_id}", "#{window_name}",
		"#{pane_id}", "#{pane_current_command}", "#{pane_dead}",
		"#{pane_width}", "#{pane_height}",
	}, sep)
	out, err := run("list-panes", "-a", "-F", format)
	if err != nil {
		// No server running means no panes, not an error for callers.
		if strings.Contains(out, "no server running") || strings.Contains(out, "error connecting") {
			return nil, nil
		}
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) != 8 {
			continue
		}
		w, _ := strconv.Atoi(f[6])
		h, _ := strconv.Atoi(f[7])
		panes = append(panes, Pane{
			Session: f[0], WindowID: f[1], WindowName: f[2],
			PaneID: f[3], Command: f[4], Dead: f[5] == "1",
			Width: w, Height: h,
		})
	}
	return panes, nil
}

func EnsureSession(name string) error {
	if _, err := run("has-session", "-t", "="+name); err == nil {
		return nil
	}
	_, err := run("new-session", "-d", "-s", name)
	return err
}

// NewWindow starts cmd in a new detached window and returns its ids.
func NewWindow(session, name string, cmd []string) (windowID, paneID string, err error) {
	args := []string{
		"new-window", "-d", "-t", session + ":", "-n", name,
		"-P", "-F", "#{window_id}" + sep + "#{pane_id}",
	}
	args = append(args, cmd...)
	out, err := run(args...)
	if err != nil {
		return "", "", err
	}
	f := strings.Split(strings.TrimSpace(out), sep)
	if len(f) != 2 {
		return "", "", fmt.Errorf("unexpected new-window output: %q", out)
	}
	// A dead pane's remains would block reuse of the name; make sure the
	// window closes on its own only when we kill it explicitly.
	run("set-option", "-t", f[0], "remain-on-exit", "off")
	return f[0], f[1], nil
}

func KillWindow(windowID string) error {
	_, err := run("kill-window", "-t", windowID)
	return err
}

// SendLiteral types text into a pane exactly as given.
func SendLiteral(paneID, text string) error {
	_, err := run("send-keys", "-t", paneID, "-l", "--", text)
	return err
}

// SendKey sends a tmux key name (e.g. "C-c", "Escape", "Up", "Enter").
func SendKey(paneID, key string) error {
	_, err := run("send-keys", "-t", paneID, key)
	return err
}

// Capture returns the last `lines` of a pane. With escapes=true the output
// preserves SGR sequences so xterm.js renders colors.
func Capture(paneID string, lines int, escapes bool) (string, error) {
	args := []string{"capture-pane", "-p", "-t", paneID, "-S", fmt.Sprintf("-%d", lines)}
	if escapes {
		args = append(args, "-e")
	}
	return run(args...)
}

// PipeStart mirrors all pane output (raw bytes) into file until PipeStop.
func PipeStart(paneID, file string) error {
	_, err := run("pipe-pane", "-t", paneID, "-o", "cat >> "+shellQuote(file))
	return err
}

func PipeStop(paneID string) error {
	_, err := run("pipe-pane", "-t", paneID)
	return err
}

// ResizeWindow sets a detached window's size so programs see the same
// dimensions as the browser terminal.
func ResizeWindow(windowID string, w, h int) error {
	if w < 20 || h < 5 || w > 500 || h > 300 {
		return fmt.Errorf("implausible size %dx%d", w, h)
	}
	_, err := run("resize-window", "-t", windowID, "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
	return err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
