// Package notify watches app state and posts crash notifications to an
// ntfy topic (https://ntfy.sh — self-hostable, no account, phone push via
// their app or web). It reads only state files and pid liveness — no tmux
// calls — so the watch loop stays nearly free even under proot.
package notify

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"proot/internal/config"
	"proot/internal/state"
)

const (
	interval      = 5 * time.Second
	crashCooldown = 5 * time.Minute // per app, for plain crashes
	bodyLines     = 6               // last words included in the message
)

type appSeen struct {
	status     string
	lastNotify time.Time
}

type Notifier struct {
	store *state.Store
	seen  map[string]appSeen
	post  func(topic, title, body, priority string) // swapped in tests
}

func New(store *state.Store) *Notifier {
	n := &Notifier{store: store, seen: map[string]appSeen{}}
	n.post = n.httpPost
	return n
}

// Run blocks; call in a goroutine. It is the agent's only always-on loop.
func (n *Notifier) Run() {
	// Prime without notifying so an agent restart doesn't re-announce
	// every app that crashed last week.
	n.tick(false)
	for {
		time.Sleep(interval)
		n.tick(true)
	}
}

func (n *Notifier) tick(notify bool) {
	settings, err := state.LoadSettings()
	if err != nil || settings.NtfyTopic == "" {
		notify = false
	}
	apps, err := n.store.Load()
	if err != nil {
		return
	}
	current := map[string]bool{}
	for _, app := range apps {
		current[app.ID] = true
		status := liteStatus(app.ID)
		prev, known := n.seen[app.ID]
		if status == prev.status {
			continue
		}
		entry := appSeen{status: status, lastNotify: prev.lastNotify}
		if notify && known && notifiable(status) {
			if status != "crashed" || time.Since(prev.lastNotify) > crashCooldown {
				title, body, prio := n.compose(app, status)
				n.post(settings.NtfyTopic, title, body, prio)
				entry.lastNotify = time.Now()
			}
		}
		n.seen[app.ID] = entry
	}
	for id := range n.seen {
		if !current[id] {
			delete(n.seen, id)
		}
	}
}

func notifiable(status string) bool {
	return status == "crashed" || status == "crashloop" || status == "killed"
}

// liteStatus mirrors the supervisor's state machine using only files and
// pid checks — good enough to detect the three notifiable transitions.
func liteStatus(id string) string {
	rs, err := state.ReadRunState(id)
	if err != nil || rs == nil {
		return "created"
	}
	alive := pidAlive(rs.PID)
	switch {
	case rs.EndedAt == nil && alive:
		return "running"
	case rs.EndedAt == nil && !alive && !pidAlive(rs.WrapperPID):
		return "killed"
	case rs.EndedAt == nil:
		return "running" // exit being recorded right now
	case rs.Breaker:
		return "crashloop"
	case state.IsStopped(id):
		return "stopped"
	case rs.ExitCode != nil && *rs.ExitCode == 0 && rs.Signal == "":
		return "exited"
	default:
		return "crashed"
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func (n *Notifier) compose(app state.App, status string) (title, body, prio string) {
	rs, _ := state.ReadRunState(app.ID)
	switch status {
	case "crashloop":
		title = fmt.Sprintf("%s is crash-looping — proot gave up", app.Name)
		prio = "high"
	case "killed":
		title = fmt.Sprintf("%s was killed by the OS", app.Name)
		prio = "high"
	default:
		title = fmt.Sprintf("%s crashed", app.Name)
		if rs != nil && rs.ExitCode != nil {
			title = fmt.Sprintf("%s crashed (exit %d)", app.Name, *rs.ExitCode)
		} else if rs != nil && rs.Signal != "" {
			title = fmt.Sprintf("%s crashed (%s)", app.Name, rs.Signal)
		}
		prio = "default"
	}
	if b, err := os.ReadFile(config.CrashFile(app.ID)); err == nil {
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) > bodyLines {
			lines = lines[len(lines)-bodyLines:]
		}
		body = strings.Join(lines, "\n")
	}
	if body == "" {
		body = "No crash log captured."
	}
	return title, body, prio
}

func (n *Notifier) httpPost(topic, title, body, priority string) {
	req, err := http.NewRequest("POST", topic, strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Tags", "rotating_light")
	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
	}
}
