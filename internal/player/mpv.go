package player

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Mpv is an Engine backed by a dedicated mpv process, controlled over its
// JSON IPC socket. mpv is found in ./bin (next to the executable) or PATH.
type Mpv struct {
	bin       string
	pipeName  string
	cmd       *exec.Cmd
	conn      net.Conn
	events    chan Event
	mu        sync.Mutex
	status    Status
	lastPause bool

	// IPC request/response plumbing
	reqMu   sync.Mutex
	nextReq int64
	readers sync.WaitGroup
	closed  bool
	startT  time.Time
}

// NewMpv locates mpv and returns an unstarted engine.
func NewMpv() (*Mpv, error) {
	bases := []string{"."}
	if exe, err := os.Executable(); err == nil {
		if dir := filepath.Dir(exe); dir != "." {
			bases = append(bases, dir)
		}
	}
	var bin string
	for _, base := range bases {
		name := "mpv"
		if runtime.GOOS == "windows" {
			name = "mpv.exe"
		}
		c := filepath.Join(base, "bin", name)
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			bin = c
			break
		}
	}
	if bin == "" {
		cand := "mpv"
		if runtime.GOOS == "windows" {
			cand = "mpv.exe"
		}
		if p, err := exec.LookPath(cand); err == nil {
			bin = p
		}
	}
	if bin == "" {
		return nil, fmt.Errorf("mpv not found (expected in ./bin or PATH)")
	}
	return &Mpv{
		bin:      bin,
		pipeName: defaultPipeName(os.Getpid()),
		events:   make(chan Event, 16),
		status:   Status{Volume: 100},
	}, nil
}

// Start spawns mpv idle and connects to its IPC socket. Safe to call again.
func (m *Mpv) Start() error {
	m.mu.Lock()
	if m.conn != nil {
		m.mu.Unlock()
		return nil
	}
	vol := m.status.Volume
	m.mu.Unlock()

	killOrphans() // mpv orphaned by a previous Wax run

	args := []string{
		"--no-video",
		"--no-terminal",
		"--idle=yes",         // stay alive between tracks
		"--audio-display=no", // don't pop a window for audio metadata
		"--force-window=no",
		"--keep-open=no",      // on end-of-track go back to idle
		"--media-controls=no", // Wax itself owns the SMTC session (internal/smtc)
		fmt.Sprintf("--input-ipc-server=%s", m.pipeName),
		fmt.Sprintf("--volume=%d", vol),
	}
	cmd := exec.Command(m.bin, args...)
	hideChildConsole(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting mpv: %w", err)
	}
	bindToLifetime(cmd.Process)
	recordMpvPID(cmd.Process.Pid)
	m.cmd = cmd
	m.startT = time.Now()

	// Connect with retries; mpv creates the pipe shortly after startup.
	var conn net.Conn
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, err := dialMpvPipe(m.pipeName)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			return fmt.Errorf("could not connect to mpv IPC (%s): %w", m.pipeName, err)
		}
		time.Sleep(150 * time.Millisecond)
	}

	m.mu.Lock()
	m.conn = conn
	m.readers.Add(1)
	go m.readLoop(conn)
	// Observe the properties the UI needs (request_id 1..4).
	for i, prop := range []string{"time-pos", "duration", "pause", "eof-reached"} {
		m.send(map[string]any{"command": []any{"observe_property", i + 1, prop}})
	}
	m.mu.Unlock()

	go func() {
		cmd.Wait() // reap on Close/quit
		m.mu.Lock()
		defer m.mu.Unlock()
		m.status.State = StateIdle
		m.status.URL = ""
		m.status.Position = 0
		m.status.Duration = 0
		m.conn = nil
	}()
	return nil
}

// Play loads the URL as the current file and starts playback. It also
// clears a paused state left over from the previous track, otherwise mpv
// would load the new file while paused and appear silent.
func (m *Mpv) Play(url string) error {
	if err := m.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.URL = url
	m.status.Position = 0
	m.status.State = StatePlaying
	m.lastPause = false
	if err := m.send(map[string]any{"command": []any{"set_property", "pause", false}}); err != nil {
		return err
	}
	return m.send(map[string]any{"command": []any{"loadfile", url, "replace"}})
}

func (m *Mpv) SetPaused(paused bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastPause = paused
	return m.send(map[string]any{"command": []any{"set_property", "pause", paused}})
}

func (m *Mpv) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.URL = ""
	m.status.State = StateIdle
	m.status.Position = 0
	m.status.Duration = 0
	return m.send(map[string]any{"command": []any{"stop"}})
}

func (m *Mpv) Seek(pos time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.send(map[string]any{"command": []any{"seek", pos.Seconds(), "absolute"}})
}

func (m *Mpv) SetVolume(v int) error {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Volume = v
	if m.conn == nil { // remember for Start()
		return nil
	}
	return m.send(map[string]any{"command": []any{"set_property", "volume", v}})
}

func (m *Mpv) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status
	return s
}

func (m *Mpv) Events() <-chan Event { return m.events }

// Close terminates mpv and the connection.
func (m *Mpv) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	conn := m.conn
	cmd := m.cmd
	m.mu.Unlock()

	if conn != nil {
		m.send(map[string]any{"command": []any{"quit"}})
		time.Sleep(150 * time.Millisecond)
		conn.Close()
	}
	if cmd != nil && cmd.Process != nil {
		go func() {
			time.Sleep(2 * time.Second)
			if cmd.ProcessState == nil {
				cmd.Process.Kill()
			}
		}()
	}
	return nil
}

// send writes a JSON command line. Caller must hold m.mu.
func (m *Mpv) send(msg map[string]any) error {
	if m.conn == nil || m.closed {
		return fmt.Errorf("mpv engine not connected")
	}
	m.nextReq++
	msg["request_id"] = m.nextReq
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := m.conn.Write(append(b, '\n')); err != nil {
		return err
	}
	return nil
}

// ipcMsg is a subset of mpv's JSON IPC messages.
type ipcMsg struct {
	Event     string          `json:"event"`
	RequestID int64           `json:"request_id"`
	Error     string          `json:"error"`
	Name      string          `json:"name"`
	Data      json.RawMessage `json:"data"`
	Reason    string          `json:"reason"`
}

func (m *Mpv) readLoop(conn net.Conn) {
	defer m.readers.Done()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var msg ipcMsg
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			continue
		}
		m.handle(&msg)
	}
	m.push(Event{Kind: EventStopped, Err: sc.Err()})
}

func (m *Mpv) handle(msg *ipcMsg) {
	switch msg.Event {
	case "property-change":
		switch msg.Name {
		case "time-pos":
			if f, ok := atof(msg.Data); ok {
				m.mu.Lock()
				m.status.Position = time.Duration(f * float64(time.Second))
				m.mu.Unlock()
			}
		case "duration":
			if f, ok := atof(msg.Data); ok {
				m.mu.Lock()
				m.status.Duration = time.Duration(f * float64(time.Second))
				m.mu.Unlock()
			}
		case "pause":
			var b bool
			if json.Unmarshal(msg.Data, &b) == nil {
				m.mu.Lock()
				prev := m.status.State
				m.status.State = map[bool]State{true: StatePaused, false: StatePlaying}[b]
				url := m.status.URL
				newState := m.status.State
				wasPause := m.lastPause
				m.lastPause = b
				m.mu.Unlock()
				if prev != newState {
					k := EventPaused
					if b == false && wasPause {
						k = EventResumed
					} else if b == false {
						k = EventPlaying
					}
					m.push(Event{Kind: k, URL: url})
				}
			}
		case "eof-reached":
			var b bool
			if json.Unmarshal(msg.Data, &b) == nil && b {
				m.mu.Lock()
				m.status.State = StateIdle
				url := m.status.URL
				m.status.URL = ""
				m.status.Position = 0
				m.status.Duration = 0
				m.mu.Unlock()
				m.push(Event{Kind: EventStopped, URL: url})
			}
		}
	case "end-file":
		// reason 0=eof 2=stop/error variants arrive as strings in newer mpv
		if msg.Reason == "eof" {
			m.mu.Lock()
			m.status.State = StateIdle
			url := m.status.URL
			m.status.URL = ""
			m.status.Position = 0
			m.status.Duration = 0
			m.mu.Unlock()
			m.push(Event{Kind: EventStopped, URL: url})
		}
	}
}

func (m *Mpv) push(e Event) {
	select {
	case m.events <- e:
	default: // listener too slow; transitions are recoverable via Status()
	}
}

func atof(raw json.RawMessage) (float64, bool) {
	s := string(raw)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, true
	}
	return 0, false
}
