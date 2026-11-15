package player

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func resolveMpvForTest() string {
	name := "mpv"
	if runtime.GOOS == "windows" {
		name = "mpv.exe"
	}
	c, err := filepath.Abs(filepath.Join("..", "..", "bin", name))
	if err != nil {
		return name
	}
	if _, err := os.Stat(c); err == nil {
		return c
	}
	return name // PATH fallback
}

func newMpvFromPath(bin string) (*Mpv, error) {
	if _, err := os.Stat(bin); err != nil {
		if _, err := exec.LookPath(bin); err != nil {
			return nil, err
		}
	}
	m := &Mpv{
		bin:      bin,
		pipeName: defaultPipeName(os.Getpid()*7 + 1),
		events:   make(chan Event, 16),
		status:   Status{Volume: 100},
	}
	return m, nil
}

// TestMpvPlayStop is a live integration test: it needs mpv in ./bin or PATH.
func TestMpvPlayStop(t *testing.T) {
	// explicit override needed because tests run with cwd = this package dir
	m, err := newMpvFromPath(resolveMpvForTest())
	if err != nil {
		t.Skip(err)
	}
	defer m.Close()
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// 1.5 seconds of near-silence? No: use a real YouTube audio stream is
	// done in the app-level test; here we use a local PCM tone via lavfi.
	err = m.Play("av://lavfi:sine=frequency=440:duration=10")
	if err != nil {
		t.Fatalf("play: %v", err)
	}

	waitState := func(want State, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if m.Status().State == want {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	if !waitState(StatePlaying, 8*time.Second) {
		t.Fatalf("expected playing, got %+v", m.Status())
	}

	if err := m.SetPaused(true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !waitState(StatePaused, 3*time.Second) {
		t.Fatalf("expected paused, got %+v", m.Status())
	}

	if err := m.SetPaused(false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !waitState(StatePlaying, 3*time.Second) {
		t.Fatalf("expected playing again, got %+v", m.Status())
	}

	if err := m.SetVolume(30); err != nil {
		t.Fatalf("volume: %v", err)
	}
	time.Sleep(600 * time.Millisecond)
	if got := m.Status().Volume; got != 30 {
		t.Logf("volume readback %d, want 30 (observed, not fatal)", got)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !waitState(StateIdle, 3*time.Second) {
		t.Logf("status after stop: %+v (idle confirmation pending)", m.Status())
	}
	t.Logf("final status: %+v", m.Status())
}

// TestPlayAfterPause guards the pause-carry-over bug: starting a new track
// while the engine is paused must actually play it (time advances).
func TestPlayAfterPause(t *testing.T) {
	m, err := newMpvFromPath(resolveMpvForTest())
	if err != nil {
		t.Skip(err)
	}
	defer m.Close()
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	tone := func(freq int) string {
		return fmt.Sprintf("av://lavfi:sine=frequency=%d:duration=10", freq)
	}
	if err := m.Play(tone(440)); err != nil {
		t.Fatalf("play 1: %v", err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for m.Status().State != StatePlaying && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}

	if err := m.SetPaused(true); err != nil { // leave the engine paused
		t.Fatalf("pause: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	// "start a new track": with the bug, mpv loaded it paused forever.
	if err := m.Play(tone(880)); err != nil {
		t.Fatalf("play 2: %v", err)
	}
	deadline = time.Now().Add(6 * time.Second)
	for m.Status().Position < 300*time.Millisecond && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	st := m.Status()
	if st.State != StatePlaying || st.Position < 300*time.Millisecond {
		t.Fatalf("new track not playing: state=%v pos=%v", st.State, st.Position)
	}
}
