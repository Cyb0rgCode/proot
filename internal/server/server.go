// Package server exposes proot's web UI and API: bearer-token auth,
// a status/CRUD API, a status event stream, and interactive terminals
// bridged to tmux panes over websockets.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"

	"proot/internal/config"
	"proot/internal/state"
	"proot/internal/supervisor"
	"proot/internal/tmuxctl"
)

type Server struct {
	sup   *supervisor.Supervisor
	token string
	terms *termStreams
	webFS fs.FS
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Token auth is the boundary; the Origin header is meaningless for
	// LAN/tunnel access where hostnames vary.
	CheckOrigin: func(r *http.Request) bool { return true },
}

func New(webFS fs.FS) (*Server, error) {
	if err := config.EnsureDirs(); err != nil {
		return nil, err
	}
	sup, err := supervisor.New()
	if err != nil {
		return nil, err
	}
	token, err := loadOrCreateToken()
	if err != nil {
		return nil, err
	}
	return &Server{sup: sup, token: token, terms: newTermStreams(), webFS: webFS}, nil
}

func loadOrCreateToken() (string, error) {
	b, err := os.ReadFile(config.TokenFile())
	if err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	if err := os.WriteFile(config.TokenFile(), []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// ListenAndServe starts the server and prints connect instructions + QR.
func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(s.webFS)))
	mux.HandleFunc("GET /api/status", s.auth(s.handleStatus))
	mux.HandleFunc("POST /api/apps", s.auth(s.handleCreateApp))
	mux.HandleFunc("PATCH /api/apps/{id}", s.auth(s.handlePatchApp))
	mux.HandleFunc("DELETE /api/apps/{id}", s.auth(s.handleDeleteApp))
	mux.HandleFunc("POST /api/apps/{id}/action", s.auth(s.handleAction))
	mux.HandleFunc("GET /api/apps/{id}/crash", s.auth(s.handleCrashLog))
	mux.HandleFunc("POST /api/adopt", s.auth(s.handleAdopt))
	mux.HandleFunc("POST /api/restart-killed", s.auth(s.handleRestartKilled))
	mux.HandleFunc("GET /api/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /api/term", s.auth(s.handleTerm))

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.printWelcome(ln.Addr().(*net.TCPAddr))
	return http.Serve(ln, mux)
}

func (s *Server) printWelcome(a *net.TCPAddr) {
	urls := []string{}
	if a.IP.IsUnspecified() {
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:%d/#t=%s", a.Port, s.token))
		for _, ip := range lanIPs() {
			urls = append(urls, fmt.Sprintf("http://%s:%d/#t=%s", ip, a.Port, s.token))
		}
	} else {
		urls = append(urls, fmt.Sprintf("http://%s/#t=%s", a.String(), s.token))
	}
	fmt.Println()
	fmt.Println("  proot is up. Open on this machine:")
	fmt.Println("    " + urls[0])
	if len(urls) > 1 {
		fmt.Println("  Or scan from your phone (same network):")
		fmt.Println("    " + urls[len(urls)-1])
		if qr, err := qrcode.New(urls[len(urls)-1], qrcode.Low); err == nil {
			fmt.Println(qr.ToSmallString(false))
		}
	} else {
		fmt.Println("  (listening on localhost only — pass --listen 0.0.0.0:PORT for LAN access,")
		fmt.Println("   or reach it through Tailscale / an SSH tunnel)")
	}
	fmt.Println("  Token file: " + config.TokenFile())
	fmt.Println()
}

func lanIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		out = append(out, ipn.IP.String())
	}
	return out
}

// --- auth ---

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		token = strings.TrimPrefix(token, "Bearer ")
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// --- REST handlers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := s.sup.Snapshot(r.URL.Query().Get("full") == "1")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, snap)
}

type createAppReq struct {
	Name        string            `json:"name"`
	Cmd         string            `json:"cmd"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
	Autorestart string            `json:"autorestart"`
	Start       *bool             `json:"start"`
}

var validRestart = regexp.MustCompile(`^(off|on-crash|always)$`)

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req createAppReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Cmd = strings.TrimSpace(req.Cmd)
	if req.Name == "" || req.Cmd == "" {
		writeErr(w, 400, errors.New("name and cmd are required"))
		return
	}
	if req.Autorestart == "" {
		req.Autorestart = "off"
	}
	if !validRestart.MatchString(req.Autorestart) {
		writeErr(w, 400, errors.New("autorestart must be off, on-crash, or always"))
		return
	}
	id := state.SlugID(req.Name)
	if _, exists, _ := s.sup.Store.Get(id); exists {
		writeErr(w, 409, fmt.Errorf("an app with id %q already exists", id))
		return
	}
	app := state.App{
		ID: id, Name: req.Name, Cmd: req.Cmd, Cwd: req.Cwd,
		Env: req.Env, Autorestart: req.Autorestart,
	}
	if err := s.sup.Store.Upsert(app); err != nil {
		writeErr(w, 500, err)
		return
	}
	if req.Start == nil || *req.Start {
		if err := s.sup.Start(id); err != nil {
			writeErr(w, 500, fmt.Errorf("app saved but failed to start: %w", err))
			return
		}
	}
	writeJSON(w, 201, app)
}

func (s *Server) handlePatchApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app, ok, err := s.sup.Store.Get(id)
	if err != nil || !ok {
		writeErr(w, 404, fmt.Errorf("no such app: %s", id))
		return
	}
	var patch map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, 400, err)
		return
	}
	apply := func(key string, dst any) error {
		if raw, ok := patch[key]; ok {
			return json.Unmarshal(raw, dst)
		}
		return nil
	}
	if err := errors.Join(
		apply("name", &app.Name),
		apply("cmd", &app.Cmd),
		apply("cwd", &app.Cwd),
		apply("env", &app.Env),
		apply("autorestart", &app.Autorestart),
		apply("pinned", &app.Pinned),
	); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !validRestart.MatchString(app.Autorestart) {
		writeErr(w, 400, errors.New("autorestart must be off, on-crash, or always"))
		return
	}
	if err := s.sup.Store.Upsert(app); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, app)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	if err := s.sup.Delete(r.PathValue("id")); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Action string `json:"action"`
		Signal string `json:"signal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	var err error
	switch req.Action {
	case "start":
		err = s.sup.Start(id)
	case "stop":
		err = s.sup.Stop(id)
	case "restart":
		err = s.sup.Restart(id)
	case "signal":
		sig, ok := map[string]syscall.Signal{
			"INT": syscall.SIGINT, "TERM": syscall.SIGTERM,
			"HUP": syscall.SIGHUP, "KILL": syscall.SIGKILL,
			"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
		}[strings.TrimPrefix(req.Signal, "SIG")]
		if !ok {
			writeErr(w, 400, fmt.Errorf("unsupported signal %q", req.Signal))
			return
		}
		err = s.sup.SendSignal(id, sig)
	default:
		writeErr(w, 400, fmt.Errorf("unknown action %q", req.Action))
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleCrashLog(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !state.ValidID(id) {
		writeErr(w, 400, errors.New("bad id"))
		return
	}
	b, err := os.ReadFile(config.CrashFile(id))
	if err != nil {
		writeErr(w, 404, errors.New("no crash log"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

type adoptReq struct {
	PaneID      string `json:"pane_id"`
	Name        string `json:"name"`
	Cmd         string `json:"cmd"`
	Cwd         string `json:"cwd"`
	Autorestart string `json:"autorestart"`
	// Takeover (default true) kills the adopted window and relaunches the
	// command under the wrapper right away. False stores the definition
	// only and leaves the window running unsupervised until you start it.
	Takeover *bool `json:"takeover"`
}

// handleAdopt turns an unmanaged tmux window into a managed app. The
// process itself can't be re-parented into the wrapper (no reptyr), so
// adoption means: save a definition and, with takeover, restart the
// command under supervision.
func (s *Server) handleAdopt(w http.ResponseWriter, r *http.Request) {
	var req adoptReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	pane, ok := findPane(req.PaneID)
	if !ok {
		writeErr(w, 404, errors.New("pane not found"))
		return
	}
	// Refuse to adopt a window proot already manages.
	if apps, err := s.sup.Store.Load(); err == nil {
		for _, a := range apps {
			if rs, _ := state.ReadRunState(a.ID); rs != nil && rs.WindowID == pane.WindowID {
				writeErr(w, 409, fmt.Errorf("that window already belongs to app %q", a.ID))
				return
			}
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = pane.WindowName
	}
	req.Cmd = strings.TrimSpace(req.Cmd)
	if req.Cmd == "" {
		writeErr(w, 400, errors.New("cmd is required (the pane's command couldn't be guessed — fill it in)"))
		return
	}
	if req.Autorestart == "" {
		req.Autorestart = "off"
	}
	if !validRestart.MatchString(req.Autorestart) {
		writeErr(w, 400, errors.New("autorestart must be off, on-crash, or always"))
		return
	}
	id := state.SlugID(req.Name)
	if _, exists, _ := s.sup.Store.Get(id); exists {
		writeErr(w, 409, fmt.Errorf("an app with id %q already exists", id))
		return
	}
	app := state.App{
		ID: id, Name: req.Name, Cmd: req.Cmd, Cwd: req.Cwd,
		Autorestart: req.Autorestart,
	}
	if err := s.sup.Store.Upsert(app); err != nil {
		writeErr(w, 500, err)
		return
	}
	takeover := req.Takeover == nil || *req.Takeover
	if takeover {
		// Kill first so the old and new copies never run concurrently.
		if err := tmuxctl.KillWindow(pane.WindowID); err != nil {
			writeErr(w, 500, fmt.Errorf("app saved but the old window could not be closed: %w", err))
			return
		}
		if err := s.sup.Start(id); err != nil {
			writeErr(w, 500, fmt.Errorf("app saved and old window closed, but start failed: %w", err))
			return
		}
	}
	writeJSON(w, 201, map[string]any{"app": app, "takeover": takeover})
}

func (s *Server) handleRestartKilled(w http.ResponseWriter, r *http.Request) {
	snap, err := s.sup.Snapshot(false)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	restarted := []string{}
	for _, id := range snap.Killed {
		if err := s.sup.Start(id); err == nil {
			restarted = append(restarted, id)
		}
	}
	writeJSON(w, 200, map[string]any{"restarted": restarted})
}

// --- websockets ---

// handleEvents pushes a status snapshot every 2s while a client is
// connected. The agent does no tmux polling at all with zero clients.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	go func() { // drain (and detect close)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		snap, err := s.sup.Snapshot(true)
		if err == nil {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(snap); err != nil {
				return
			}
		}
		<-ticker.C
	}
}

type termClientMsg struct {
	T    string `json:"t"` // input | key | resize
	Data string `json:"data,omitempty"`
	Key  string `json:"key,omitempty"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// Named keys the key bar may send; everything else arrives as raw bytes.
var allowedKeys = map[string]bool{
	"Enter": true, "Escape": true, "Tab": true, "BTab": true, "BSpace": true,
	"Up": true, "Down": true, "Left": true, "Right": true,
	"S-Up": true, "S-Down": true, "S-Left": true, "S-Right": true,
	"C-Up": true, "C-Down": true, "C-Left": true, "C-Right": true,
	"PPage": true, "NPage": true, "Home": true, "End": true,
	"C-c": true, "C-d": true, "C-z": true, "C-l": true, "C-r": true,
}

// handleTerm bridges one websocket to one tmux pane: history via
// capture-pane, live output via pipe-pane, input via send-keys -H.
func (s *Server) handleTerm(w http.ResponseWriter, r *http.Request) {
	paneID := r.URL.Query().Get("pane")
	if paneID == "" {
		if id := r.URL.Query().Get("app"); id != "" {
			if rs, _ := state.ReadRunState(id); rs != nil {
				paneID = rs.PaneID
			}
		}
	}
	pane, ok := findPane(paneID)
	if !ok {
		http.Error(w, `{"error":"pane not found"}`, 404)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	out, cancel, err := s.terms.Subscribe(paneID)
	if err != nil {
		conn.WriteJSON(map[string]string{"t": "error", "msg": err.Error()})
		return
	}
	defer cancel()

	// Scrollback first, so the terminal doesn't open onto a void.
	if hist, err := tmuxctl.Capture(paneID, 300, true); err == nil {
		conn.WriteJSON(map[string]string{"t": "data",
			"data": strings.ReplaceAll(hist, "\n", "\r\n") + "\r\n"})
	}

	done := make(chan struct{})
	go func() { // pane output → client
		defer close(done)
		for data := range out {
			conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := conn.WriteJSON(map[string]string{"t": "data", "data": string(data)}); err != nil {
				conn.Close()
				return
			}
		}
	}()

	for { // client input → pane
		var msg termClientMsg
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.T {
		case "input":
			if len(msg.Data) > 0 && len(msg.Data) <= 16*1024 {
				sendHexChunked(paneID, []byte(msg.Data))
			}
		case "key":
			if allowedKeys[msg.Key] {
				tmuxctl.SendKey(paneID, msg.Key)
			}
		case "resize":
			tmuxctl.ResizeWindow(pane.WindowID, msg.Cols, msg.Rows)
		}
	}
	cancel()
	<-done
}

func findPane(paneID string) (tmuxctl.Pane, bool) {
	if paneID == "" || !strings.HasPrefix(paneID, "%") {
		return tmuxctl.Pane{}, false
	}
	panes, err := tmuxctl.ListPanes()
	if err != nil {
		return tmuxctl.Pane{}, false
	}
	for _, p := range panes {
		if p.PaneID == paneID {
			return p, true
		}
	}
	return tmuxctl.Pane{}, false
}
