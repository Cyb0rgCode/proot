// Package sched fires scheduled apps. Each app may carry a 5-field cron
// expression; at every matching minute the scheduler starts the app unless
// it is already running. Typical use: a scraper with autorestart off and
// schedule "0 3 * * *" runs nightly, exits cleanly, and waits in its pane
// until the next firing.
package sched

import (
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"phoned/internal/state"
)

var parser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

type Starter interface {
	Start(id string) error
}

type Scheduler struct {
	store   *state.Store
	starter Starter
	now     func() time.Time // swapped in tests
}

func New(store *state.Store, starter Starter) *Scheduler {
	return &Scheduler{store: store, starter: starter, now: time.Now}
}

// Run blocks; call in a goroutine. It sleeps to each minute boundary and
// fires whatever matches — no per-app timers to leak or resync.
func (s *Scheduler) Run() {
	for {
		now := s.now()
		next := now.Truncate(time.Minute).Add(time.Minute)
		time.Sleep(next.Sub(now) + 500*time.Millisecond)
		s.FireDue(next)
	}
}

// FireDue starts every scheduled app whose cron expression matches the
// given minute. Exposed for tests.
func (s *Scheduler) FireDue(minute time.Time) {
	apps, err := s.store.Load()
	if err != nil {
		return
	}
	minute = minute.Truncate(time.Minute)
	for _, app := range apps {
		if app.Schedule == "" {
			continue
		}
		sched, err := parser.Parse(app.Schedule)
		if err != nil {
			continue
		}
		// The expression matches this minute iff Next from one second
		// before lands exactly on it.
		if !sched.Next(minute.Add(-time.Second)).Equal(minute) {
			continue
		}
		if running(app.ID) {
			continue
		}
		s.starter.Start(app.ID)
	}
}

func running(id string) bool {
	rs, err := state.ReadRunState(id)
	if err != nil || rs == nil || rs.EndedAt != nil {
		return false
	}
	return pidAlive(rs.PID)
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return sysKill(pid) == nil
}

func sysKill(pid int) error {
	err := syscall.Kill(pid, 0)
	if err == syscall.EPERM {
		return nil
	}
	return err
}
