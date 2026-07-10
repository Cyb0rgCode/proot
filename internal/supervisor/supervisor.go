// Package supervisor is the agent-side brain: it merges three sources of
// truth — apps.json, the wrapper-written state files, and live tmux panes —
// into app statuses, and implements start/stop/restart/delete on top of tmux.
package supervisor

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"

	"proot/internal/config"
	"proot/internal/state"
	"proot/internal/tmuxctl"
)

// Status values surfaced to the UI.
const (
	StatusRunning    = "running"
	StatusExited     = "exited"    // clean exit, wrapper prompt showing
	StatusCrashed    = "crashed"   // non-zero exit or signal
	StatusCrashLoop  = "crashloop" // circuit breaker tripped
	StatusStopped    = "stopped"   // user asked for it to stay down
	StatusKilled     = "killed"    // no ended_at, pids gone, pane gone: the OS murdered it
	StatusNotStarted = "created"   // defined but never launched
)

type AppStatus struct {
	state.App
	Status       string     `json:"status"`
	PID          int        `json:"pid,omitempty"`
	WindowID     string     `json:"window_id,omitempty"`
	PaneID       string     `json:"pane_id,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	ExitCode     *int       `json:"exit_code,omitempty"`
	Signal       string     `json:"signal,omitempty"`
	UptimeSec    int64      `json:"uptime_sec,omitempty"`
	RestartCount int        `json:"restart_count"`
	HasCrashLog  bool       `json:"has_crash_log"`
	LastLine     string     `json:"last_line,omitempty"`
}

type Snapshot struct {
	Apps      []AppStatus    `json:"apps"`
	Unmanaged []tmuxctl.Pane `json:"unmanaged"`
	// Killed lists app ids currently in the "killed by the OS" state,
	// feeding the dashboard's restart-all banner.
	Killed []string  `json:"killed"`
	Time   time.Time `json:"time"`
}

type Supervisor struct {
	Store *state.Store
	Exe   string // path to the proot binary, for wrapper invocations
}

func New() (*Supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &Supervisor{Store: state.NewStore(), Exe: exe}, nil
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Snapshot computes the full dashboard state. withLastLine controls the
// per-app capture-pane call (one tmux exec per running app — skip it for
// cheap polls).
func (s *Supervisor) Snapshot(withLastLine bool) (*Snapshot, error) {
	apps, err := s.Store.Load()
	if err != nil {
		return nil, err
	}
	panes, err := tmuxctl.ListPanes()
	if err != nil {
		return nil, err
	}
	paneByID := map[string]tmuxctl.Pane{}
	managedWindows := map[string]bool{}
	for _, p := range panes {
		paneByID[p.PaneID] = p
	}

	snap := &Snapshot{Time: time.Now()}
	for _, app := range apps {
		st := s.appStatus(app, paneByID)
		if st.WindowID != "" {
			managedWindows[st.WindowID] = true
		}
		if st.Status == StatusKilled {
			snap.Killed = append(snap.Killed, app.ID)
		}
		if withLastLine && st.PaneID != "" {
			if out, err := tmuxctl.Capture(st.PaneID, 5, false); err == nil {
				st.LastLine = lastNonEmptyLine(out)
			}
		}
		snap.Apps = append(snap.Apps, st)
	}
	sort.Slice(snap.Apps, func(i, j int) bool {
		if snap.Apps[i].Pinned != snap.Apps[j].Pinned {
			return snap.Apps[i].Pinned
		}
		return snap.Apps[i].Name < snap.Apps[j].Name
	})
	for _, p := range panes {
		if !managedWindows[p.WindowID] {
			snap.Unmanaged = append(snap.Unmanaged, p)
		}
	}
	return snap, nil
}

func (s *Supervisor) appStatus(app state.App, paneByID map[string]tmuxctl.Pane) AppStatus {
	st := AppStatus{App: app, Status: StatusNotStarted}
	rs, err := state.ReadRunState(app.ID)
	if err != nil || rs == nil {
		return st
	}
	st.PID = rs.PID
	st.StartedAt = &rs.StartedAt
	st.EndedAt = rs.EndedAt
	st.ExitCode = rs.ExitCode
	st.Signal = rs.Signal
	st.RestartCount = len(rs.Launches) - 1
	if st.RestartCount < 0 {
		st.RestartCount = 0
	}
	if _, err := os.Stat(config.CrashFile(app.ID)); err == nil {
		st.HasCrashLog = true
	}

	pane, paneAlive := paneByID[rs.PaneID]
	if paneAlive {
		st.WindowID, st.PaneID = pane.WindowID, rs.PaneID
	}
	stopped := state.IsStopped(app.ID)

	switch {
	case paneAlive && rs.EndedAt == nil:
		// Child (or a just-died child the wrapper is about to record) —
		// report running; the wrapper updates the file within milliseconds.
		st.Status = StatusRunning
		st.UptimeSec = int64(time.Since(rs.StartedAt).Seconds())
	case paneAlive: // wrapper prompt is showing
		switch {
		case rs.Breaker:
			st.Status = StatusCrashLoop
		case stopped:
			st.Status = StatusStopped
		case rs.ExitCode != nil && *rs.ExitCode == 0 && rs.Signal == "":
			st.Status = StatusExited
		default:
			st.Status = StatusCrashed
		}
	case rs.EndedAt == nil && !pidAlive(rs.PID) && !pidAlive(rs.WrapperPID):
		// The signature state: it was running, nothing recorded an exit,
		// and every process is gone. Android (or the OOM killer) did this.
		st.Status = StatusKilled
	case rs.EndedAt == nil:
		// tmux gone but processes somehow alive — treat as killed; Start()
		// cleans up the survivors before relaunching.
		st.Status = StatusKilled
	case stopped:
		st.Status = StatusStopped
	case rs.ExitCode != nil && *rs.ExitCode == 0 && rs.Signal == "":
		st.Status = StatusExited
	default:
		st.Status = StatusCrashed
	}
	return st
}

// Start launches an app (or un-pauses a wrapper sitting at its prompt).
func (s *Supervisor) Start(id string) error {
	app, ok, err := s.Store.Get(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such app: %s", id)
	}
	state.SetStopped(id, false)

	rs, _ := state.ReadRunState(id)
	if rs != nil {
		if pane := livePane(rs.PaneID); pane {
			if rs.EndedAt == nil && pidAlive(rs.PID) {
				return nil // already running
			}
			// Wrapper is at its prompt (or in a backoff wait): poke it.
			return tmuxctl.SendLiteral(rs.PaneID, "r")
		}
		// Survivors from a dead tmux would double-run the app.
		if pidAlive(rs.PID) {
			syscall.Kill(rs.PID, syscall.SIGKILL)
		}
		if pidAlive(rs.WrapperPID) {
			syscall.Kill(rs.WrapperPID, syscall.SIGKILL)
		}
	}

	if err := tmuxctl.EnsureSession(config.SessionName); err != nil {
		return err
	}
	// --dir pins the wrapper to the agent's state directory: the window
	// inherits env from the tmux server, which may have been started by a
	// user shell with a different (or no) PROOT_DIR.
	_, _, err = tmuxctl.NewWindow(config.SessionName, app.ID,
		[]string{s.Exe, "run", "--id", app.ID, "--dir", config.Dir()})
	return err
}

// Stop terminates the child and flags the app to stay down. The wrapper
// stays alive showing the exit status.
func (s *Supervisor) Stop(id string) error {
	if err := state.SetStopped(id, true); err != nil {
		return err
	}
	rs, _ := state.ReadRunState(id)
	if rs == nil || rs.EndedAt != nil || !pidAlive(rs.PID) {
		return nil
	}
	syscall.Kill(rs.PID, syscall.SIGTERM)
	go escalate(id, rs.PID, rs.StartedAt)
	return nil
}

// escalate SIGKILLs a child that ignored SIGTERM for 5 seconds.
func escalate(id string, pid int, startedAt time.Time) {
	time.Sleep(5 * time.Second)
	rs, _ := state.ReadRunState(id)
	if rs != nil && rs.PID == pid && rs.StartedAt.Equal(startedAt) &&
		rs.EndedAt == nil && pidAlive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}

// Restart = stop (without the stay-down flag) + start once it's down.
func (s *Supervisor) Restart(id string) error {
	rs, _ := state.ReadRunState(id)
	if rs == nil || rs.EndedAt != nil || !pidAlive(rs.PID) {
		return s.Start(id)
	}
	syscall.Kill(rs.PID, syscall.SIGTERM)
	go func(pid int, startedAt time.Time) {
		escalateDeadline := time.Now().Add(7 * time.Second)
		for time.Now().Before(escalateDeadline) {
			cur, _ := state.ReadRunState(id)
			if cur == nil || cur.EndedAt != nil || !cur.StartedAt.Equal(startedAt) {
				break
			}
			if time.Since(startedAt) > 5*time.Second && pidAlive(pid) {
				syscall.Kill(pid, syscall.SIGKILL)
			}
			time.Sleep(200 * time.Millisecond)
		}
		s.Start(id)
	}(rs.PID, rs.StartedAt)
	return nil
}

// SendSignal delivers a signal to the running child (e.g. SIGINT for Ctrl-C
// semantics at the supervision level; the terminal view sends real keys).
func (s *Supervisor) SendSignal(id string, sig syscall.Signal) error {
	rs, _ := state.ReadRunState(id)
	if rs == nil || rs.EndedAt != nil || !pidAlive(rs.PID) {
		return fmt.Errorf("app %s is not running", id)
	}
	return syscall.Kill(rs.PID, sig)
}

// Delete stops the app, closes its window, and removes definition + state.
func (s *Supervisor) Delete(id string) error {
	rs, _ := state.ReadRunState(id)
	if rs != nil {
		if pidAlive(rs.PID) {
			syscall.Kill(rs.PID, syscall.SIGKILL)
		}
		if rs.WindowID != "" && livePane(rs.PaneID) {
			tmuxctl.KillWindow(rs.WindowID)
		}
	}
	state.RemoveRunState(id)
	return s.Store.Delete(id)
}

func livePane(paneID string) bool {
	if paneID == "" {
		return false
	}
	panes, err := tmuxctl.ListPanes()
	if err != nil {
		return false
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return true
		}
	}
	return false
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			if len(t) > 160 {
				t = t[:160]
			}
			return t
		}
	}
	return ""
}
