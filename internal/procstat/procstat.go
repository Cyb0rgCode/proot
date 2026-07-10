// Package procstat samples CPU and memory for an app's whole process
// subtree from /proc. The app's recorded pid is `sh -c ...`; the real work
// usually happens in its descendants, so single-pid numbers would lie.
package procstat

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var pageSize = int64(os.Getpagesize())

// clockTicks is USER_HZ; 100 on every Linux that matters here.
const clockTicks = 100

type proc struct {
	pid, ppid int
	jiffies   uint64 // utime + stime
	rssPages  int64
}

// scan reads every /proc/<pid>/stat once. One pass serves all apps.
func scan() map[int]proc {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	procs := make(map[int]proc, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		// comm may contain spaces/parens: fields resume after the last ')'.
		i := strings.LastIndexByte(s, ')')
		if i < 0 || i+2 > len(s) {
			continue
		}
		f := strings.Fields(s[i+2:])
		// After comm+state: f[1]=ppid f[11]=utime f[12]=stime f[21]=rss
		if len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		ut, _ := strconv.ParseUint(f[11], 10, 64)
		st, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseInt(f[21], 10, 64)
		procs[pid] = proc{pid: pid, ppid: ppid, jiffies: ut + st, rssPages: rss}
	}
	return procs
}

// subtree returns root plus all transitive children present in procs.
func subtree(procs map[int]proc, root int) []proc {
	children := map[int][]int{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	var out []proc
	stack := []int{root}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		p, ok := procs[pid]
		if !ok {
			continue
		}
		out = append(out, p)
		stack = append(stack, children[pid]...)
	}
	return out
}

// Usage is one app's resource reading.
type Usage struct {
	CPUPercent float64
	MemBytes   int64
}

// Sampler computes CPU% from jiffie deltas between successive Sample calls,
// so the first reading for an app reports memory only.
type Sampler struct {
	mu   sync.Mutex
	last map[int]samplePoint
}

type samplePoint struct {
	jiffies uint64
	at      time.Time
}

func NewSampler() *Sampler {
	return &Sampler{last: map[int]samplePoint{}}
}

// Sample returns usage per requested root pid. Roots that no longer exist
// are absent from the result and forgotten.
func (s *Sampler) Sample(roots []int) map[int]Usage {
	procs := scan()
	if procs == nil {
		return nil
	}
	now := time.Now()
	out := make(map[int]Usage, len(roots))

	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[int]bool{}
	for _, root := range roots {
		tree := subtree(procs, root)
		if len(tree) == 0 {
			continue
		}
		seen[root] = true
		var jiffies uint64
		var rss int64
		for _, p := range tree {
			jiffies += p.jiffies
			rss += p.rssPages
		}
		u := Usage{MemBytes: rss * pageSize}
		if prev, ok := s.last[root]; ok && now.After(prev.at) && jiffies >= prev.jiffies {
			dt := now.Sub(prev.at).Seconds()
			u.CPUPercent = float64(jiffies-prev.jiffies) / clockTicks / dt * 100
		}
		s.last[root] = samplePoint{jiffies: jiffies, at: now}
		out[root] = u
	}
	for pid := range s.last {
		if !seen[pid] {
			delete(s.last, pid)
		}
	}
	return out
}
