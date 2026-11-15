//go:build windows

// Package smtc — Windows System Media Transport Controls, client side.
//
// The raw WinRT/COM interop lives in an isolated helper process
// (cmd/smtc-helper, built as wax-smtc.exe) that this package supervises over
// a stdio JSON protocol. Rationale: the in-process interop randomly brought
// the whole gio UI down (silent 0xB00 exits); in a child process a COM fault
// costs the media session, never the app. The helper exits when stdin closes,
// so no stale process or SMTC session survives the app, and this shim
// respawns it on death, replaying the latest cached state.
package smtc

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Button int

const (
	BtnPlay     Button = 0
	BtnPause    Button = 1
	BtnStop     Button = 2
	BtnNext     Button = 6
	BtnPrevious Button = 7
)

func (b Button) String() string {
	switch b {
	case BtnPlay:
		return "Play"
	case BtnPause:
		return "Pause"
	case BtnStop:
		return "Stop"
	case BtnNext:
		return "Next"
	case BtnPrevious:
		return "Previous"
	}
	return "Button"
}

// Replay-cache slots (in apply order after a respawn).
const (
	slotTrack = iota
	slotCover
	slotPlayback
	slotEnable
	slotCount
)

// client supervises one helper process. All public setters are safe from any
// goroutine and no-op while no helper is running.
type client struct {
	mu    sync.Mutex
	alive bool
	proc  *exec.Cmd
	pipe  *os.File

	cache [slotCount]string // latest command per slot, "" = never set

	fastFails int // consecutive helper deaths under 2s -> give up

	helperPath string
	onButton   func(Button)
}

var cur *client

// Start locates and spawns the helper. Returns false when unavailable;
// setters then stay silent no-ops.
func Start(onButton func(Button)) bool {
	path := findHelper()
	if path == "" {
		return false
	}
	c := &client{helperPath: path, onButton: onButton}
	if !c.spawn() {
		return false
	}
	cur = c
	return true
}

func findHelper() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "wax-smtc.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "wax-smtc.exe" // fall back to PATH
}

// snapshot returns the replay cache in slot order, skipping empty slots.
func (c *client) snapshot() []string {
	var out []string
	for _, line := range c.cache {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

func (c *client) send(slot int, line string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if !c.alive {
		if slot >= 0 {
			c.cache[slot] = line // remember for a future respawn
		}
		c.mu.Unlock()
		return
	}
	if slot >= 0 {
		c.cache[slot] = line
	}
	c.mu.Unlock()
	c.deliver(line)
}

// deliver writes one line to the current pipe; a write error surfaces as a
// pipe close, which the reaper handles (respawn + replay).
func (c *client) deliver(line string) {
	c.mu.Lock()
	pipe := c.pipe
	c.mu.Unlock()
	if pipe == nil {
		return
	}
	if _, err := pipe.Write([]byte(line + "\n")); err != nil {
		return
	}
}

func (c *client) spawn() bool {
	cmd := exec.Command(c.helperPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	in, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	pipe := in.(*os.File)
	replay := func() []string {
		c.mu.Lock()
		c.proc, c.pipe, c.alive = cmd, pipe, true
		snap := c.snapshot()
		c.mu.Unlock()
		return snap
	}()
	go readLoop(c, out)
	go func() {
		start := time.Now()
		cmd.Wait()
		c.mu.Lock()
		wasAlive := c.alive
		c.alive = false
		c.pipe = nil
		stillCurrent := cur == c
		c.mu.Unlock()
		pipe.Close()
		if !wasAlive || !stillCurrent {
			return
		}
		if time.Since(start) < 2*time.Second {
			c.mu.Lock()
			c.fastFails++
			giveUp := c.fastFails >= 3
			c.mu.Unlock()
			if giveUp {
				return
			}
		} else {
			c.mu.Lock()
			c.fastFails = 0
			c.mu.Unlock()
		}
		// helper died while we are the active supervisor: respawn + replay
		c.mu.Lock()
		stillCurrent = cur == c
		c.mu.Unlock()
		if stillCurrent {
			c.spawn()
		}
	}()
	// replay latest state so a respawn never drops the track/metadata
	for _, line := range replay {
		c.deliver(line)
	}
	return true
}

func readLoop(c *client, out interface{ Read([]byte) (int, error) }) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 4096), 1<<22)
	type ev struct {
		E   string `json:"e"`
		Btn int    `json:"btn"`
	}
	for sc.Scan() {
		var e ev
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.E == "button" && c.onButton != nil {
			c.onButton(Button(e.Btn))
		}
	}
}

// marshal builds one protocol line from alternating key/value string pairs.
func marshal(kv ...string) string {
	var b []byte
	b = append(b, '{')
	for i := 0; i+1 < len(kv); i += 2 {
		if len(b) > 1 {
			b = append(b, ',')
		}
		k, _ := json.Marshal(kv[i])
		v, _ := json.Marshal(kv[i+1])
		b = append(append(b, k...), ':')
		b = append(b, v...)
	}
	return string(append(b, '}'))
}

// SetTrack publishes title/artist/album plus the cover file path.
func SetTrack(title, artist, album, coverPath string) {
	cur.send(slotTrack, marshal("c", "track", "title", title, "artist", artist, "album", album, "cover", coverPath))
}

func SetCover(coverPath string) {
	cur.send(slotCover, marshal("c", "cover", "path", coverPath))
}

func SetPlayback(playing bool) {
	cmd := `{"c":"pause"}`
	if playing {
		cmd = `{"c":"play"}`
	}
	cur.send(slotPlayback, cmd)
}

func SetStopped() {
	cur.send(slotPlayback, `{"c":"stop"}`)
}

// SetTimeline takes Go durations (ns); the helper converts to WinRT ticks.
func SetTimeline(pos, dur time.Duration) {
	if dur <= 0 {
		return
	}
	b, _ := json.Marshal(map[string]interface{}{"c": "timeline", "pos": int64(pos), "dur": int64(dur)})
	cur.send(-1, string(b)) // high-frequency: not cached
}

func Enable(on bool) {
	b, _ := json.Marshal(map[string]interface{}{"c": "enable", "on": on})
	cur.send(slotEnable, string(b))
}
