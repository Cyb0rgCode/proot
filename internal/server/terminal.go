package server

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"phoned/internal/config"
	"phoned/internal/tmuxctl"
)

// termStreams multiplexes live pane output to any number of websocket
// viewers. One pipe-pane per pane, refcounted; output bytes flow
// pane → pipe file → tail loop → subscriber channels.
type termStreams struct {
	mu      sync.Mutex
	streams map[string]*paneStream
}

type paneStream struct {
	paneID string
	file   string
	subs   map[chan []byte]bool
	stop   chan struct{}
}

func newTermStreams() *termStreams {
	return &termStreams{streams: map[string]*paneStream{}}
}

// Subscribe attaches to a pane's output. The returned channel receives raw
// terminal bytes; call the returned cancel func exactly once.
func (t *termStreams) Subscribe(paneID string) (<-chan []byte, func(), error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	ps, ok := t.streams[paneID]
	if !ok {
		file := filepath.Join(config.PipeDir(), "pane-"+strings.TrimPrefix(paneID, "%")+".out")
		os.Remove(file)
		if f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
		if err := tmuxctl.PipeStart(paneID, file); err != nil {
			os.Remove(file)
			return nil, nil, err
		}
		ps = &paneStream{
			paneID: paneID,
			file:   file,
			subs:   map[chan []byte]bool{},
			stop:   make(chan struct{}),
		}
		t.streams[paneID] = ps
		go ps.tail()
	}

	ch := make(chan []byte, 64)
	ps.subs[ch] = true

	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(ps.subs, ch)
		if len(ps.subs) == 0 {
			close(ps.stop)
			delete(t.streams, paneID)
			tmuxctl.PipeStop(paneID)
			os.Remove(ps.file)
		}
	}
	return ch, cancel, nil
}

// tail polls the pipe file for appended bytes. 100ms latency is invisible
// next to network latency, and polling one fd is cheap even under proot.
func (ps *paneStream) tail() {
	var offset int64
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ps.stop:
			return
		case <-ticker.C:
		}
		f, err := os.Open(ps.file)
		if err != nil {
			continue
		}
		for {
			n, err := f.ReadAt(buf, offset)
			if n > 0 {
				offset += int64(n)
				data := make([]byte, n)
				copy(data, buf[:n])
				ps.broadcast(data)
			}
			if err != nil || n == 0 {
				break
			}
		}
		f.Close()
	}
}

func (ps *paneStream) broadcast(data []byte) {
	for ch := range ps.subs {
		select {
		case ch <- data:
		default: // slow client: drop rather than stall every viewer
		}
	}
}

// sendHexChunked injects raw bytes into a pane as typed keys via
// `send-keys -H`, which round-trips control characters and UTF-8 exactly.
// Chunked to keep argv well under any command-line limit.
func sendHexChunked(paneID string, data []byte) error {
	const chunk = 256
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		args := []string{"send-keys", "-H", "-t", paneID}
		for _, b := range data[off:end] {
			args = append(args, hex.EncodeToString([]byte{b}))
		}
		if err := tmuxctl.Raw(args...); err != nil {
			return err
		}
	}
	return nil
}
