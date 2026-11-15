// Package player defines the playback engine interface used by the GUI.
// The mpv implementation (Engine over mpv's JSON IPC) is the first backend;
// others can be added without touching the UI code.
package player

import "time"

// State is the coarse playback state.
type State int

const (
	StateIdle State = iota
	StatePlaying
	StatePaused
)

func (s State) String() string {
	switch s {
	case StatePlaying:
		return "playing"
	case StatePaused:
		return "paused"
	default:
		return "idle"
	}
}

// Status is a snapshot of the engine state for rendering.
type Status struct {
	State    State
	URL      string // currently loaded URL, empty when idle
	Position time.Duration
	Duration time.Duration
	Volume   int // 0..100
}

// Kind of a notification event sent on the Events channel.
type Kind int

const (
	EventPlaying Kind = iota
	EventPaused
	EventResumed
	EventStopped // stopped, ended, or error
)

// Event signals a state transition to listeners.
type Event struct {
	Kind Kind
	URL  string
	Err  error
}

// Engine is a music playback backend.
type Engine interface {
	// Start prepares the backend (spawn process, connect, ...). Idempotent.
	Start() error
	// Play (re)loads the given URL and starts playback.
	Play(url string) error
	// SetPaused pauses or resumes the current playback.
	SetPaused(paused bool) error
	// Stop aborts the current playback and unloads it.
	Stop() error
	// Seek jumps to an absolute position in the current file.
	Seek(pos time.Duration) error
	// SetVolume sets the volume, 0..100.
	SetVolume(v int) error
	// Status returns a snapshot of the current state.
	Status() Status
	// Events emits state transitions (Playing/Paused/Resumed/Stopped).
	Events() <-chan Event
	// Close shuts the backend down.
	Close() error
}
