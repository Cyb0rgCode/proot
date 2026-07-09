// Package state owns muxboard's two persistence surfaces: the app
// definitions in apps.json and the per-app runtime state files.
//
// Write discipline (this is what keeps the design race-free):
//   - apps.json is written only by the agent (API), read by the wrapper.
//   - state/<id>.json is written only by the wrapper, read by the agent.
//   - state/<id>.stopped is created/removed only by the agent, read by the wrapper.
//
// All writes are atomic (temp file + rename).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"muxboard/internal/config"
)

// App is a user-defined program muxboard manages.
type App struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Cmd         string            `json:"cmd"` // run via `sh -c`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Autorestart string            `json:"autorestart"` // "off" | "on-crash" | "always"
	Pinned      bool              `json:"pinned,omitempty"`
}

// RunState is written by the wrapper as the app runs. One file per app.
type RunState struct {
	ID         string     `json:"id"`
	PID        int        `json:"pid"` // wrapper's child (the actual app)
	WrapperPID int        `json:"wrapper_pid"`
	WindowID   string     `json:"window_id"` // tmux @n
	PaneID     string     `json:"pane_id"`   // tmux %n
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Signal     string     `json:"signal,omitempty"`
	// Launches holds recent child launch times (bounded); the wrapper's
	// circuit breaker and the UI's restart counter both read from it.
	Launches []time.Time `json:"launches,omitempty"`
	Breaker  bool        `json:"breaker,omitempty"` // crash loop: wrapper gave up
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// SlugID derives a filesystem/tmux-safe id from a display name.
func SlugID(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = regexp.MustCompile(`[^a-z0-9_-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-_")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = fmt.Sprintf("app-%d", time.Now().Unix()%100000)
	}
	return s
}

func ValidID(id string) bool { return validID.MatchString(id) }

// Store is the apps.json accessor. Safe for concurrent use within one process.
type Store struct {
	mu sync.Mutex
}

func NewStore() *Store { return &Store{} }

func (s *Store) Load() ([]App, error) {
	b, err := os.ReadFile(config.AppsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var apps []App
	if err := json.Unmarshal(b, &apps); err != nil {
		return nil, fmt.Errorf("apps.json is corrupt: %w", err)
	}
	return apps, nil
}

func (s *Store) Get(id string) (App, bool, error) {
	apps, err := s.Load()
	if err != nil {
		return App{}, false, err
	}
	for _, a := range apps {
		if a.ID == id {
			return a, true, nil
		}
	}
	return App{}, false, nil
}

// Upsert adds or replaces an app definition.
func (s *Store) Upsert(app App) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	apps, err := s.Load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range apps {
		if apps[i].ID == app.ID {
			apps[i] = app
			replaced = true
			break
		}
	}
	if !replaced {
		apps = append(apps, app)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ID < apps[j].ID })
	return writeJSONAtomic(config.AppsFile(), apps)
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	apps, err := s.Load()
	if err != nil {
		return err
	}
	out := apps[:0]
	for _, a := range apps {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return writeJSONAtomic(config.AppsFile(), out)
}

// --- runtime state files ---

func ReadRunState(id string) (*RunState, error) {
	b, err := os.ReadFile(config.StateFile(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var rs RunState
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	return &rs, nil
}

func WriteRunState(rs *RunState) error {
	// Keep the launch history bounded.
	if n := len(rs.Launches); n > 20 {
		rs.Launches = rs.Launches[n-20:]
	}
	return writeJSONAtomic(config.StateFile(rs.ID), rs)
}

func RemoveRunState(id string) {
	os.Remove(config.StateFile(id))
	os.Remove(config.CrashFile(id))
	os.Remove(config.StoppedFlag(id))
}

// Stopped flag: "the user asked for this app to stay down".
func SetStopped(id string, stopped bool) error {
	if stopped {
		return os.WriteFile(config.StoppedFlag(id), nil, 0o600)
	}
	err := os.Remove(config.StoppedFlag(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func IsStopped(id string) bool {
	_, err := os.Stat(config.StoppedFlag(id))
	return err == nil
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
