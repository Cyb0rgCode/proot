// Package config resolves muxboard's on-disk layout.
//
//	~/.muxboard/
//	  token          bearer token for the web UI/API
//	  apps.json      app definitions (the only file a user should hand-edit)
//	  state/<id>.json    runtime state, written only by the `muxboard run` wrapper
//	  state/<id>.crash   last ~200 pane lines captured on a non-zero exit
//	  state/<id>.stopped flag: user asked for this app to stay down
//	  pipe/          transient pipe-pane output files for live terminals
package config

import (
	"os"
	"path/filepath"
)

// SessionName is the tmux session managed app windows are created in.
const SessionName = "muxboard"

func Dir() string {
	if d := os.Getenv("MUXBOARD_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".muxboard")
}

func AppsFile() string             { return filepath.Join(Dir(), "apps.json") }
func TokenFile() string            { return filepath.Join(Dir(), "token") }
func StateDir() string             { return filepath.Join(Dir(), "state") }
func PipeDir() string              { return filepath.Join(Dir(), "pipe") }
func StateFile(id string) string   { return filepath.Join(StateDir(), id+".json") }
func CrashFile(id string) string   { return filepath.Join(StateDir(), id+".crash") }
func StoppedFlag(id string) string { return filepath.Join(StateDir(), id+".stopped") }

func EnsureDirs() error {
	for _, d := range []string{Dir(), StateDir(), PipeDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}
